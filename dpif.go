package main

import (
	"syscall"
	"fmt"
	"unsafe"
)

const (
	DATAPATH = iota
	VPORT = iota
	FLOW = iota
	PACKET = iota
	FAMILY_COUNT = iota
)

var familyNames = [FAMILY_COUNT]string {
	"ovs_datapath",
	"ovs_vport",
	"ovs_flow",
	"ovs_packet",
}

type Dpif struct {
	sock *NetlinkSocket
	familyIds [FAMILY_COUNT]uint16
}

func lookupFamily(sock *NetlinkSocket, name string) (uint16, error) {
	id, err := sock.LookupGenlFamily(name)
	if err == nil {
		return id, nil
	}

	if err == NetlinkError(syscall.ENOENT) {
		return 0, fmt.Errorf("Generic netlink family '%s' unavailable; the Open vSwitch kernel module is probably not loaded", name)
	}

	return 0, err
}

func NewDpif() (*Dpif, error) {
	sock, err := OpenNetlinkSocket(syscall.NETLINK_GENERIC)
        if err != nil {
                return nil, err
        }

	dpif := &Dpif{sock: sock}

	for i := 0; i < FAMILY_COUNT; i++ {
		dpif.familyIds[i], err = lookupFamily(sock, familyNames[i])
		if err != nil {
			sock.Close()
			return nil, err
		}
	}

	return dpif, nil
}

func (dpif *Dpif) Close() error {
	if dpif.sock == nil { return nil }
	err := dpif.sock.Close()
	dpif.sock = nil
	return err
}

func ovsHeaderAt(data []byte, pos int) *OvsHeader {
	return (*OvsHeader)(unsafe.Pointer(&data[pos]))
}

func (nlmsg *NlMsgBuilder) PutOvsHeader(ifindex int32) {
	pos := nlmsg.AlignGrow(syscall.NLMSG_ALIGNTO, SizeofOvsHeader)
	h := ovsHeaderAt(nlmsg.buf, pos)
	h.DpIfIndex = ifindex
}

func (nlmsg *NlMsgParser) TakeOvsHeader() (*OvsHeader, error) {
	pos, err := nlmsg.AlignAdvance(syscall.NLMSG_ALIGNTO, SizeofOvsHeader)
	if err != nil {
		return nil, err
	}

	return ovsHeaderAt(nlmsg.data, pos), nil
}

type datapathInfo struct {
	ifindex int32
	name string
}

func (dpif *Dpif) parseDatapathInfo(msg *NlMsgParser) (res datapathInfo, err error) {
	_, err = msg.ExpectNlMsghdr(dpif.familyIds[DATAPATH])
	if err != nil { return }

	_, err = msg.ExpectGenlMsghdr(OVS_DP_CMD_NEW)
	if err != nil { return }

	ovshdr, err := msg.TakeOvsHeader()
	if err != nil { return }
	res.ifindex = ovshdr.DpIfIndex

	attrs, err := msg.TakeAttrs()
	if err != nil { return }

	res.name, err = attrs.GetString(OVS_DP_ATTR_NAME)
	return
}

type Datapath struct {
	dpif *Dpif
	ifindex int32
}

func (dpif *Dpif) CreateDatapath(name string) (*Datapath, error) {
	var features uint32 = OVS_DP_F_UNALIGNED | OVS_DP_F_VPORT_PIDS

	req := NewNlMsgBuilder(RequestFlags, dpif.familyIds[DATAPATH])
	req.PutGenlMsghdr(OVS_DP_CMD_NEW, OVS_DATAPATH_VERSION)
	req.PutOvsHeader(0)
	req.PutStringAttr(OVS_DP_ATTR_NAME, name)
	req.PutUint32Attr(OVS_DP_ATTR_UPCALL_PID, 0)
	req.PutUint32Attr(OVS_DP_ATTR_USER_FEATURES, features)

	resp, err := dpif.sock.Request(req)
	if err != nil {
		return nil, err
	}

	dpi, err := dpif.parseDatapathInfo(resp)
	if err != nil {
		return nil, err
	}

	return &Datapath{dpif: dpif, ifindex: dpi.ifindex}, nil
}

func (dpif *Dpif) LookupDatapath(name string) (*Datapath, error) {
	req := NewNlMsgBuilder(RequestFlags, dpif.familyIds[DATAPATH])
	req.PutGenlMsghdr(OVS_DP_CMD_GET, OVS_DATAPATH_VERSION)
	req.PutOvsHeader(0)
	req.PutStringAttr(OVS_DP_ATTR_NAME, name)

	resp, err := dpif.sock.Request(req)
	if err != nil {
		if err == NetlinkError(syscall.ENODEV) {
			// no datapath with the given name
			return nil, nil
		}

		return nil, err
	}

	dpi, err := dpif.parseDatapathInfo(resp)
	if err != nil {
		return nil, err
	}

	return &Datapath{dpif: dpif, ifindex: dpi.ifindex}, nil
}

func (dpif *Dpif) EnumerateDatapaths() (map[string]*Datapath, error) {
	res := make(map[string]*Datapath)

	req := NewNlMsgBuilder(DumpFlags, dpif.familyIds[DATAPATH])
	req.PutGenlMsghdr(OVS_DP_CMD_GET, OVS_DATAPATH_VERSION)
	req.PutOvsHeader(0)

	consumer := func (resp *NlMsgParser) error {
		dpi, err := dpif.parseDatapathInfo(resp)
		if err != nil {	return err }
		res[dpi.name] = &Datapath{dpif: dpif, ifindex: dpi.ifindex}
		return nil
	}

	err := dpif.sock.RequestMulti(req, consumer)
	if err != nil {
		return nil, err
	}

	return res, nil
}

func (dp *Datapath) Delete() error {
	req := NewNlMsgBuilder(RequestFlags, dp.dpif.familyIds[DATAPATH])
	req.PutGenlMsghdr(OVS_DP_CMD_DEL, OVS_DATAPATH_VERSION)
	req.PutOvsHeader(dp.ifindex)

	_, err := dp.dpif.sock.Request(req)
	if err != nil {	return err }

	dp.dpif = nil
	dp.ifindex = -1
	return nil
}

type portInfo struct {
	portNo uint32
	name string
}

func (dp *Datapath) parsePortInfo(msg *NlMsgParser) (res portInfo, err error) {
	_, err = msg.ExpectNlMsghdr(dp.dpif.familyIds[VPORT])
	if err != nil { return }

	_, err = msg.ExpectGenlMsghdr(OVS_VPORT_CMD_NEW)
	if err != nil { return }

	ovshdr, err := msg.TakeOvsHeader()
	if err != nil { return }

	if ovshdr.DpIfIndex != dp.ifindex {
		err = fmt.Errorf("wrong datapath ifindex in response (got %d, expected %d)", ovshdr.DpIfIndex, dp.ifindex)
		return
	}

	attrs, err := msg.TakeAttrs()
	if err != nil { return }

	res.portNo, err = attrs.GetUint32(OVS_VPORT_ATTR_PORT_NO)
	if err != nil { return }

	res.name, err = attrs.GetString(OVS_VPORT_ATTR_NAME)
	return
}

type Port struct {
	datapath *Datapath
	portNo uint32
}

func (dp *Datapath) CreatePort(name string) (*Port, error) {
	dpif := dp.dpif

	req := NewNlMsgBuilder(RequestFlags, dpif.familyIds[VPORT])
	req.PutGenlMsghdr(OVS_VPORT_CMD_NEW, OVS_VPORT_VERSION)
	req.PutOvsHeader(dp.ifindex)
	req.PutStringAttr(OVS_VPORT_ATTR_NAME, name)
	req.PutUint32Attr(OVS_VPORT_ATTR_TYPE, OVS_VPORT_TYPE_INTERNAL)
	req.PutUint32Attr(OVS_VPORT_ATTR_UPCALL_PID, dpif.sock.Pid())

	resp, err := dpif.sock.Request(req)
	if err != nil {
		return nil, err
	}

	pi, err := dp.parsePortInfo(resp)
	if err != nil {
		return nil, err
	}

	return &Port{datapath: dp, portNo: pi.portNo}, nil
}

func (dp *Datapath) LookupPort(name string) (*Port, error) {
	dpif := dp.dpif

	req := NewNlMsgBuilder(RequestFlags, dpif.familyIds[VPORT])
	req.PutGenlMsghdr(OVS_VPORT_CMD_GET, OVS_VPORT_VERSION)
	req.PutOvsHeader(dp.ifindex)
	req.PutStringAttr(OVS_VPORT_ATTR_NAME, name)

	resp, err := dpif.sock.Request(req)
	if err != nil {
		if err == NetlinkError(syscall.ENODEV) {
			// no port with the given name
			return nil, nil
		}

		return nil, err
	}

	pi, err := dp.parsePortInfo(resp)
	if err != nil {
		return nil, err
	}

	return &Port{datapath: dp, portNo: pi.portNo}, nil
}

func (dp *Datapath) EnumeratePorts() (map[string]*Port, error) {
	dpif := dp.dpif
	res := make(map[string]*Port)

	req := NewNlMsgBuilder(DumpFlags, dpif.familyIds[VPORT])
	req.PutGenlMsghdr(OVS_VPORT_CMD_GET, OVS_VPORT_VERSION)
	req.PutOvsHeader(dp.ifindex)

	consumer := func (resp *NlMsgParser) error {
		pi, err := dp.parsePortInfo(resp)
		if err != nil {	return err }
		res[pi.name] = &Port{datapath: dp, portNo: pi.portNo}
		return nil
	}

	err := dpif.sock.RequestMulti(req, consumer)
	if err != nil {
		return nil, err
	}

	return res, nil
}

func (port *Port) Delete() error {
	dpif := port.datapath.dpif

	req := NewNlMsgBuilder(RequestFlags, dpif.familyIds[VPORT])
	req.PutGenlMsghdr(OVS_VPORT_CMD_DEL, OVS_VPORT_VERSION)
	req.PutOvsHeader(port.datapath.ifindex)
	req.PutUint32Attr(OVS_VPORT_ATTR_PORT_NO, port.portNo)

	_, err := dpif.sock.Request(req)
	if err != nil { return err }

	port.datapath = nil
	port.portNo = ^uint32(0)
	return nil
}

type FlowKey interface {
	typeId() uint8
	toNlAttr(*NlMsgBuilder)
}

type FlowSpec struct {
	keys map[uint8]FlowKey
}

func NewFlowSpec() FlowSpec {
	return FlowSpec{keys: make(map[uint8]FlowKey)}
}

func (f FlowSpec) AddKey(k FlowKey) {
	// TODO check for collisions
	f.keys[k.typeId()] = k
}

func (f FlowSpec) toNlAttrs(msg *NlMsgBuilder) {
	msg.PutAttr(OVS_FLOW_ATTR_KEY, func () {
		for _, k := range(f.keys) {
			k.toNlAttr(msg)
		}
	})
	msg.PutAttr(OVS_FLOW_ATTR_ACTIONS, func () {
	})
}

func (a FlowSpec) Equals(b FlowSpec) bool {
	for id, ak := range(a.keys) {
		bk, ok := b.keys[id]
		if !ok || ak != bk { return false }
	}

	for id := range(b.keys) {
		_, ok := a.keys[id]
		if !ok { return false }
	}

	return true
}

func NewEthernetFlowKey(src [6]byte, dst [6]byte) FlowKey {
	return OvsKeyEthernet{EthSrc: src, EthDst: dst}
}

func (key OvsKeyEthernet) typeId() uint8 {
	return OVS_KEY_ATTR_ETHERNET
}

func (key OvsKeyEthernet) toNlAttr(msg *NlMsgBuilder) {
	p := msg.PutStructAttr(OVS_KEY_ATTR_ETHERNET, SizeofOvsKeyEthernet)
	*(*OvsKeyEthernet)(p) = key
}

func parseEthernetFlowKey(attrs Attrs) (k FlowKey, err error) {
	p, err := attrs.GetStruct(OVS_KEY_ATTR_ETHERNET, SizeofOvsKeyEthernet)
	if err != nil {
		return
	}

	k = *(*OvsKeyEthernet)(p)
	return
}

var flowKeyParsers = map[uint16](func (Attrs) (FlowKey, error)) {
	OVS_KEY_ATTR_ETHERNET: parseEthernetFlowKey,
}

func (dp *Datapath) parseFlowSpec(msg *NlMsgParser) (FlowSpec, error) {
	f := NewFlowSpec()

	_, err := msg.ExpectNlMsghdr(dp.dpif.familyIds[FLOW])
	if err != nil { return f, err }

	_, err = msg.ExpectGenlMsghdr(OVS_FLOW_CMD_NEW)
	if err != nil { return f, err }

	ovshdr, err := msg.TakeOvsHeader()
	if err != nil { return f, err }

	if ovshdr.DpIfIndex != dp.ifindex {
		err = fmt.Errorf("wrong datapath ifindex in response (got %d, expected %d)", ovshdr.DpIfIndex, dp.ifindex)
		return f, err
	}

	attrs, err := msg.TakeAttrs()
	if err != nil { return f, err }

	keys, err := attrs.GetNestedAttrs(OVS_FLOW_ATTR_KEY)
	if err != nil { return f, err}

	for typ := range(keys) {
		parser, ok := flowKeyParsers[typ]
		if !ok {
			//err = fmt.Errorf("unknown flow key type %d", typ)
			//return f, err
			continue
		}

		k, err := parser(keys)
		if err != nil {
			return f, err
		}

		f.AddKey(k)
	}

	return f, nil
}

func (dp *Datapath) CreateFlow(f FlowSpec) error {
	dpif := dp.dpif

	req := NewNlMsgBuilder(RequestFlags, dpif.familyIds[FLOW])
	req.PutGenlMsghdr(OVS_FLOW_CMD_NEW, OVS_FLOW_VERSION)
	req.PutOvsHeader(dp.ifindex)
	f.toNlAttrs(req)

	_, err := dpif.sock.Request(req)
	if err != nil {
		return err
	}

	return nil
}

type NoSuchFlowError struct {}
func (NoSuchFlowError) Error() string {	return "no such flow" }

func (dp *Datapath) DeleteFlow(f FlowSpec) error {
	dpif := dp.dpif

	req := NewNlMsgBuilder(RequestFlags, dpif.familyIds[FLOW])
	req.PutGenlMsghdr(OVS_FLOW_CMD_DEL, OVS_FLOW_VERSION)
	req.PutOvsHeader(dp.ifindex)
	f.toNlAttrs(req)

	_, err := dpif.sock.Request(req)
	if err == NetlinkError(syscall.ENOENT) {
		err = NoSuchFlowError{}
	}

	return err
}

func (dp *Datapath) EnumerateFlows() ([]FlowSpec, error) {
	dpif := dp.dpif
	res := make([]FlowSpec, 0)

	req := NewNlMsgBuilder(DumpFlags, dpif.familyIds[FLOW])
	req.PutGenlMsghdr(OVS_FLOW_CMD_GET, OVS_FLOW_VERSION)
	req.PutOvsHeader(dp.ifindex)

	consumer := func (resp *NlMsgParser) error {
		f, err := dp.parseFlowSpec(resp)
		if err != nil {	return err }
		res = append(res, f)
		return nil
	}

	err := dpif.sock.RequestMulti(req, consumer)
	if err != nil {
		return nil, err
	}

	return res, nil
}
