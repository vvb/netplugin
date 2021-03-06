/***
Copyright 2014 Cisco Systems Inc. All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package ofnet

// This file implements the virtual router functionality using Vxlan overlay

// VXLAN tables are structured as follows
//
// +-------+										 +-------------------+
// | Valid +---------------------------------------->| ARP to Controller |
// | Pkts  +-->+-------+                             +-------------------+
// +-------+   | Vlan  |        +---------+
//             | Table +------->| IP Dst  |          +--------------+
//             +-------+        | Lookup  +--------->| Ucast Output |
//                              +----------          +--------------+
//
//

import (
	//"fmt"
	"errors"
	"net"
	"net/rpc"
	"strings"

	"github.com/contiv/ofnet/ofctrl"
	"github.com/shaleman/libOpenflow/openflow13"
	"github.com/shaleman/libOpenflow/protocol"

	log "github.com/Sirupsen/logrus"
)

// Vrouter state.
// One Vrouter instance exists on each host
type Vrouter struct {
	agent       *OfnetAgent      // Pointer back to ofnet agent that owns this
	ofSwitch    *ofctrl.OFSwitch // openflow switch we are talking to
	policyAgent *PolicyAgent     // Policy agent

	// Fgraph tables
	inputTable *ofctrl.Table // Packet lookup starts here
	vlanTable  *ofctrl.Table // Vlan Table. map port or VNI to vlan
	ipTable    *ofctrl.Table // IP lookup table

	// Flow Database
	flowDb         map[string]*ofctrl.Flow // Database of flow entries
	portVlanFlowDb map[uint32]*ofctrl.Flow // Database of flow entries

	// Router Mac to be used
	myRouterMac net.HardwareAddr
}

// Create a new vrouter instance
func NewVrouter(agent *OfnetAgent, rpcServ *rpc.Server) *Vrouter {
	vrouter := new(Vrouter)

	// Keep a reference to the agent
	vrouter.agent = agent

	// Create policy agent
	vrouter.policyAgent = NewPolicyAgent(agent, rpcServ)

	// Create a flow dbs and my router mac
	vrouter.flowDb = make(map[string]*ofctrl.Flow)
	vrouter.portVlanFlowDb = make(map[uint32]*ofctrl.Flow)
	vrouter.myRouterMac, _ = net.ParseMAC("00:00:11:11:11:11")

	return vrouter
}

// Handle new master added event
func (self *Vrouter) MasterAdded(master *OfnetNode) error {

	return nil
}

// Handle switch connected notification
func (self *Vrouter) SwitchConnected(sw *ofctrl.OFSwitch) {
	// Keep a reference to the switch
	self.ofSwitch = sw

	log.Infof("Switch connected(vrouter). installing flows")

	// Tell the policy agent about the switch
	self.policyAgent.SwitchConnected(sw)

	// Init the Fgraph
	self.initFgraph()
}

// Handle switch disconnected notification
func (self *Vrouter) SwitchDisconnected(sw *ofctrl.OFSwitch) {
	// FIXME: ??
}

// Handle incoming packet
func (self *Vrouter) PacketRcvd(sw *ofctrl.OFSwitch, pkt *ofctrl.PacketIn) {
	switch pkt.Data.Ethertype {
	case 0x0806:
		if (pkt.Match.Type == openflow13.MatchType_OXM) &&
			(pkt.Match.Fields[0].Class == openflow13.OXM_CLASS_OPENFLOW_BASIC) &&
			(pkt.Match.Fields[0].Field == openflow13.OXM_FIELD_IN_PORT) {
			// Get the input port number
			switch t := pkt.Match.Fields[0].Value.(type) {
			case *openflow13.InPortField:
				var inPortFld openflow13.InPortField
				inPortFld = *t

				self.processArp(pkt.Data, inPortFld.InPort)
			}

		}

	case 0x0800:
		// FIXME: We dont expect IP packets. Use this for statefull policies.
	default:
		log.Errorf("Received unknown ethertype: %x", pkt.Data.Ethertype)
	}
}

// Add a local endpoint and install associated local route
func (self *Vrouter) AddLocalEndpoint(endpoint OfnetEndpoint) error {
	vni := self.agent.vlanVniMap[endpoint.Vlan]
	if vni == nil {
		log.Errorf("VNI for vlan %d is not known", endpoint.Vlan)
		return errors.New("Unknown Vlan")
	}

	// Install a flow entry for vlan mapping and point it to IP table
	portVlanFlow, err := self.vlanTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		InputPort: endpoint.PortNo,
	})
	if err != nil {
		log.Errorf("Error creating portvlan entry. Err: %v", err)
		return err
	}

	// Set the vlan and install it
	// FIXME: Dont set the vlan till multi-vrf support. We cant pop vlan unless flow matches on vlan
	// portVlanFlow.SetVlan(endpoint.Vlan)

	// Set source endpoint group if specified
	if endpoint.EndpointGroup != 0 {
		metadata, metadataMask := SrcGroupMetadata(endpoint.EndpointGroup)
		portVlanFlow.SetMetadata(metadata, metadataMask)
	}

	// Point it to dst group table for policy lookups
	dstGrpTbl := self.ofSwitch.GetTable(DST_GRP_TBL_ID)
	err = portVlanFlow.Next(dstGrpTbl)
	if err != nil {
		log.Errorf("Error installing portvlan entry. Err: %v", err)
		return err
	}

	// save the flow entry
	self.portVlanFlowDb[endpoint.PortNo] = portVlanFlow

	// Create the output port
	outPort, err := self.ofSwitch.OutputPort(endpoint.PortNo)
	if err != nil {
		log.Errorf("Error creating output port %d. Err: %v", endpoint.PortNo, err)
		return err
	}

	// Install the IP address
	ipFlow, err := self.ipTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		Ethertype: 0x0800,
		IpDa:      &endpoint.IpAddr,
	})
	if err != nil {
		log.Errorf("Error creating flow for endpoint: %+v. Err: %v", endpoint, err)
		return err
	}

	destMacAddr, _ := net.ParseMAC(endpoint.MacAddrStr)

	// Set Mac addresses
	ipFlow.SetMacDa(destMacAddr)
	ipFlow.SetMacSa(self.myRouterMac)

	// Point the route at output port
	err = ipFlow.Next(outPort)
	if err != nil {
		log.Errorf("Error installing flow for endpoint: %+v. Err: %v", endpoint, err)
		return err
	}

	// Install dst group entry for the endpoint
	err = self.policyAgent.AddEndpoint(&endpoint)
	if err != nil {
		log.Errorf("Error adding endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	// Store the flow
	self.flowDb[endpoint.IpAddr.String()] = ipFlow

	return nil
}

// Remove local endpoint
func (self *Vrouter) RemoveLocalEndpoint(endpoint OfnetEndpoint) error {

	// Remove the port vlan flow.
	portVlanFlow := self.portVlanFlowDb[endpoint.PortNo]
	if portVlanFlow != nil {
		err := portVlanFlow.Delete()
		if err != nil {
			log.Errorf("Error deleting portvlan flow. Err: %v", err)
		}
	}

	// Find the flow entry
	ipFlow := self.flowDb[endpoint.IpAddr.String()]
	if ipFlow == nil {
		log.Errorf("Error finding the flow for endpoint: %+v", endpoint)
		return errors.New("Flow not found")
	}

	// Delete the Fgraph entry
	err := ipFlow.Delete()
	if err != nil {
		log.Errorf("Error deleting the endpoint: %+v. Err: %v", endpoint, err)
	}

	// Remove the endpoint from policy tables
	err = self.policyAgent.DelEndpoint(&endpoint)
	if err != nil {
		log.Errorf("Error deleting endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	return nil
}

// Add virtual tunnel end point. This is mainly used for mapping remote vtep IP
// to ofp port number.
func (self *Vrouter) AddVtepPort(portNo uint32, remoteIp net.IP) error {
	// Install a flow entry for default VNI/vlan and point it to IP table
	portVlanFlow, err := self.vlanTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		InputPort: portNo,
	})
	if err != nil && strings.Contains(err.Error(), "Flow already exists") {
		log.Infof("VTEP %s already exists", remoteIp.String())
		return nil
	} else if err != nil {
		log.Errorf("Error adding Flow for VTEP %v. Err: %v", remoteIp, err)
		return err
	}

	// FIXME: Need to match on tunnelId and set vlan-id per VRF
	// FIXME: not needed till multi-vrf support. We cant pop vlan unless flow matches on vlan
	// portVlanFlow.SetVlan(1)

	// Point it to next table.
	// Note that we bypass policy lookup on dest host.
	portVlanFlow.Next(self.ipTable)

	// walk all routes and see if we need to install it
	for _, endpoint := range self.agent.endpointDb {
		if endpoint.OriginatorIp.String() == remoteIp.String() {
			err := self.AddEndpoint(endpoint)
			if err != nil {
				log.Errorf("Error installing endpoint during vtep add(%v) EP: %+v. Err: %v", remoteIp, endpoint, err)
				return err
			}
		}
	}

	return nil
}

// Remove a VTEP port
func (self *Vrouter) RemoveVtepPort(portNo uint32, remoteIp net.IP) error {
	return nil
}

// Add a vlan.
// This is mainly used for mapping vlan id to Vxlan VNI
func (self *Vrouter) AddVlan(vlanId uint16, vni uint32) error {
	// FIXME: Add this for multiple VRF support
	return nil
}

// Remove a vlan
func (self *Vrouter) RemoveVlan(vlanId uint16, vni uint32) error {
	// FIXME: Add this for multiple VRF support
	return nil
}

// AddEndpoint Add an endpoint to the datapath
func (self *Vrouter) AddEndpoint(endpoint *OfnetEndpoint) error {
	log.Infof("AddEndpoint call for endpoint: %+v", endpoint)

	// Lookup the VTEP for the endpoint
	vtepPort := self.agent.vtepTable[endpoint.OriginatorIp.String()]
	if vtepPort == nil {
		log.Warnf("Could not find the VTEP for endpoint: %+v", endpoint)

		// Return if VTEP is not found. We'll install the route when VTEP is added
		return nil
	}

	// Install the endpoint in OVS
	// Create an output port for the vtep
	outPort, err := self.ofSwitch.OutputPort(*vtepPort)
	if err != nil {
		log.Errorf("Error creating output port %d. Err: %v", *vtepPort, err)
		return err
	}

	// Install the IP address
	ipFlow, err := self.ipTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		Ethertype: 0x0800,
		IpDa:      &endpoint.IpAddr,
	})
	if err != nil {
		log.Errorf("Error creating flow for endpoint: %+v. Err: %v", endpoint, err)
		return err
	}

	// Set Mac addresses
	ipFlow.SetMacDa(self.myRouterMac)
	// This is strictly not required at the source OVS. Source mac will be
	// overwritten by the dest OVS anyway. We keep the source mac for debugging purposes..
	// ipFlow.SetMacSa(self.myRouterMac)

	// Set VNI
	// FIXME: hardcode VNI for default VRF.
	// FIXME: We need to use fabric VNI per VRF
	// FIXME: Cant pop vlan tag till the flow matches on vlan.
	ipFlow.SetTunnelId(1)

	// Point it to output port
	err = ipFlow.Next(outPort)
	if err != nil {
		log.Errorf("Error installing flow for endpoint: %+v. Err: %v", endpoint, err)
		return err
	}

	// Install dst group entry for the endpoint
	err = self.policyAgent.AddEndpoint(endpoint)
	if err != nil {
		log.Errorf("Error adding endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	// Store it in flow db
	self.flowDb[endpoint.IpAddr.String()] = ipFlow

	return nil
}

// RemoveEndpoint removes an endpoint from the datapath
func (self *Vrouter) RemoveEndpoint(endpoint *OfnetEndpoint) error {
	// Find the flow entry
	ipFlow := self.flowDb[endpoint.IpAddr.String()]
	if ipFlow == nil {
		log.Errorf("Error finding the flow for endpoint: %+v", endpoint)
		return errors.New("Flow not found")
	}

	// Delete the Fgraph entry
	err := ipFlow.Delete()
	if err != nil {
		log.Errorf("Error deleting the endpoint: %+v. Err: %v", endpoint, err)
	}

	// Remove the endpoint from policy tables
	err = self.policyAgent.DelEndpoint(endpoint)
	if err != nil {
		log.Errorf("Error deleting endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	return nil
}

// AddUplink adds an uplink to the switch
func (vr *Vrouter) AddUplink(portNo uint32) error {
	// Nothing to do
	return nil
}

// RemoveUplink remove an uplink to the switch
func (vr *Vrouter) RemoveUplink(portNo uint32) error {
	// Nothing to do
	return nil
}

// initialize Fgraph on the switch
func (self *Vrouter) initFgraph() error {
	sw := self.ofSwitch

	// Create all tables
	self.inputTable = sw.DefaultTable()
	self.vlanTable, _ = sw.NewTable(VLAN_TBL_ID)
	self.ipTable, _ = sw.NewTable(IP_TBL_ID)

	// Init policy tables
	err := self.policyAgent.InitTables(IP_TBL_ID)
	if err != nil {
		log.Fatalf("Error installing policy table. Err: %v", err)
		return err
	}

	//Create all drop entries
	// Drop mcast source mac
	bcastMac, _ := net.ParseMAC("01:00:00:00:00:00")
	bcastSrcFlow, _ := self.inputTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		MacSa:     &bcastMac,
		MacSaMask: &bcastMac,
	})
	bcastSrcFlow.Next(sw.DropAction())

	// Redirect ARP packets to controller
	arpFlow, _ := self.inputTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		Ethertype: 0x0806,
	})
	arpFlow.Next(sw.SendToController())

	// Send all valid packets to vlan table
	// This is installed at lower priority so that all packets that miss above
	// flows will match entry
	validPktFlow, _ := self.inputTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MISS_PRIORITY,
	})
	validPktFlow.Next(self.vlanTable)

	// Drop all packets that miss Vlan lookup
	vlanMissFlow, _ := self.vlanTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MISS_PRIORITY,
	})
	vlanMissFlow.Next(sw.DropAction())

	// Drop all packets that miss IP lookup
	ipMissFlow, _ := self.ipTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MISS_PRIORITY,
	})
	ipMissFlow.Next(sw.DropAction())

	return nil
}

// Process incoming ARP packets
func (self *Vrouter) processArp(pkt protocol.Ethernet, inPort uint32) {
	log.Debugf("processing ARP packet on port %d", inPort)
	switch t := pkt.Data.(type) {
	case *protocol.ARP:
		log.Debugf("ARP packet: %+v", *t)
		var arpHdr protocol.ARP = *t

		switch arpHdr.Operation {
		case protocol.Type_Request:
			// Lookup the Dest IP in the endpoint table
			endpoint := self.agent.getEndpointByIp(arpHdr.IPDst)
			if endpoint == nil {
				// If we dont know the IP address, dont send an ARP response
				log.Infof("Received ARP request for unknown IP: %v", arpHdr.IPDst)
				return
			}

			// Form an ARP response
			arpResp, _ := protocol.NewARP(protocol.Type_Reply)
			arpResp.HWSrc = self.myRouterMac
			arpResp.IPSrc = arpHdr.IPDst
			arpResp.HWDst = arpHdr.HWSrc
			arpResp.IPDst = arpHdr.IPSrc

			log.Infof("Sending ARP response: %+v", arpResp)

			// build the ethernet packet
			ethPkt := protocol.NewEthernet()
			ethPkt.HWDst = arpResp.HWDst
			ethPkt.HWSrc = arpResp.HWSrc
			ethPkt.Ethertype = 0x0806
			ethPkt.Data = arpResp

			log.Infof("Sending ARP response Ethernet: %+v", ethPkt)

			// Packet out
			pktOut := openflow13.NewPacketOut()
			pktOut.Data = ethPkt
			pktOut.AddAction(openflow13.NewActionOutput(inPort))

			log.Infof("Sending ARP response packet: %+v", pktOut)

			// Send it out
			self.ofSwitch.Send(pktOut)
		default:
			log.Infof("Dropping ARP response packet from port %d", inPort)
		}
	}
}
