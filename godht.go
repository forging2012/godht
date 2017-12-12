package godht

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"net"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/fanpei91/bencode"
)

var BootstrapNodes = []string{
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.utorrent.com:6881",
}

type nodeID []byte
type node struct {
	addr string
	id   string
}
type Announce struct {
	Raw         map[string]interface{}
	From        *net.UDPAddr
	Peer        *net.TCPAddr
	Infohash    []byte
	InfohashHex string
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func neighborID(target nodeID, local nodeID) nodeID {
	id := make([]byte, 20)
	copy(id[:10], target[:10])
	copy(id[10:], local[10:])
	return id
}

func makeQuery(tid string, q string, a map[string]interface{}) map[string]interface{} {
	dict := map[string]interface{}{
		"t": tid,
		"y": "q",
		"q": q,
		"a": a,
	}
	return dict
}

func makeReply(tid string, r map[string]interface{}) map[string]interface{} {
	dict := map[string]interface{}{
		"t": tid,
		"y": "r",
		"r": r,
	}
	return dict
}

func decodeNodes(s string) (nodes []*node) {
	length := len(s)
	if length%26 != 0 {
		return
	}
	for i := 0; i < length; i += 26 {
		id := s[i : i+20]
		ip := net.IP([]byte(s[i+20 : i+24])).String()
		port := binary.BigEndian.Uint16([]byte(s[i+24 : i+26]))
		addr := ip + ":" + strconv.Itoa(int(port))
		nodes = append(nodes, &node{id: id, addr: addr})
	}
	return
}

func per(events int, duration time.Duration) rate.Limit {
	return rate.Every(duration / time.Duration(events))
}

type GoDHT struct {
	Announce         chan *Announce
	node             chan *node
	localID          nodeID
	conn             *net.UDPConn
	queryTypes       map[string]func(map[string]interface{}, *net.UDPAddr)
	friendsLimiter   *rate.Limiter
	announceMaxCache int
	maxFriendsPerSec int
	secret           string
	bootstraps       []string
}

type option func(*GoDHT)

func MaxFriendsPerSec(n int) option {
	return func(g *GoDHT) {
		g.maxFriendsPerSec = n
	}
}

func LocalID(id []byte) option {
	return func(g *GoDHT) {
		g.localID = id
	}
}

func Secret(s string) option {
	return func(g *GoDHT) {
		g.secret = s
	}
}

func Bootstraps(addr []string) option {
	return func(g *GoDHT) {
		g.bootstraps = addr
	}
}

func New(laddr string, options ...option) (*GoDHT, error) {
	conn, err := net.ListenPacket("udp4", laddr)
	if err != nil {
		return nil, err
	}
	g := &GoDHT{
		localID:          randBytes(20),
		conn:             conn.(*net.UDPConn),
		node:             make(chan *node),
		maxFriendsPerSec: 200,
		secret:           "IYHJFR%^&IO",
		bootstraps:       BootstrapNodes,
	}
	for _, option := range options {
		option(g)
	}
	g.announceMaxCache = g.maxFriendsPerSec * 2
	g.friendsLimiter = rate.NewLimiter(per(g.maxFriendsPerSec, time.Second), g.maxFriendsPerSec)
	g.Announce = make(chan *Announce, g.announceMaxCache)
	g.queryTypes = map[string]func(map[string]interface{}, *net.UDPAddr){
		"get_peers":     g.onGetPeersQuery,
		"announce_peer": g.onAnnouncePeerQuery,
	}
	go g.listen()
	go g.join()
	go g.makefriends()
	return g, nil
}

func (g *GoDHT) listen() error {
	buf := make([]byte, 8192)
	for {
		n, addr, err := g.conn.ReadFromUDP(buf)
		if err == nil {
			go g.onMessage(buf[:n], addr)
		}
	}
}

func (g *GoDHT) join() {
	for _, addr := range g.bootstraps {
		g.node <- &node{addr: addr, id: string(randBytes(20))}
	}
}

func (g *GoDHT) onMessage(data []byte, from *net.UDPAddr) {
	dict, err := bencode.Decode(bytes.NewBuffer(data))
	if err != nil {
		return
	}
	y, ok := dict["y"].(string)
	if !ok {
		return
	}
	switch y {
	case "q":
		g.onQuery(dict, from)
	case "r", "e":
		g.onReply(dict, from)
	}
}

func (g *GoDHT) onQuery(dict map[string]interface{}, from *net.UDPAddr) {
	_, ok := dict["t"].(string)
	if !ok {
		return
	}
	q, ok := dict["q"].(string)
	if !ok {
		return
	}
	if f, ok := g.queryTypes[q]; ok {
		f(dict, from)
	}
}

func (g *GoDHT) onReply(dict map[string]interface{}, from *net.UDPAddr) {
	r, ok := dict["r"].(map[string]interface{})
	if !ok {
		return
	}
	nodes, ok := r["nodes"].(string)
	if !ok {
		return
	}
	for _, node := range decodeNodes(nodes) {
		if !g.friendsLimiter.Allow() {
			continue
		}
		g.node <- node
	}
}

func (g *GoDHT) findNode(to string, target nodeID) {
	d := makeQuery(string(randBytes(2)), "find_node", map[string]interface{}{
		"id":     string(neighborID(target, g.localID)),
		"target": string(randBytes(20)),
	})
	addr, err := net.ResolveUDPAddr("udp4", to)
	if err != nil {
		return
	}
	g.send(d, addr)
}

func (g *GoDHT) onGetPeersQuery(dict map[string]interface{}, from *net.UDPAddr) {
	t := dict["t"].(string)
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		return
	}
	id, ok := a["id"].(string)
	if !ok {
		return
	}
	d := makeReply(t, map[string]interface{}{
		"id":    string(neighborID([]byte(id), g.localID)),
		"nodes": "",
		"token": g.genToken(from),
	})
	g.send(d, from)
}

func (g *GoDHT) onAnnouncePeerQuery(dict map[string]interface{}, from *net.UDPAddr) {
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		return
	}
	token, ok := a["token"].(string)
	if !ok || !g.validateToken(token, from) {
		return
	}
	if len(g.Announce) == g.announceMaxCache {
		return
	}
	if ac := g.summarize(dict, from); ac != nil {
		g.Announce <- ac
	}
}

func (g *GoDHT) summarize(dict map[string]interface{}, from *net.UDPAddr) *Announce {
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		return nil
	}
	infohash, ok := a["info_hash"].(string)
	if !ok {
		return nil
	}
	port := int64(from.Port)
	if impliedPort, ok := a["implied_port"].(int64); ok && impliedPort == 0 {
		if p, ok := a["port"].(int64); ok {
			port = p
		}
	}
	return &Announce{
		Raw:         dict,
		From:        from,
		Infohash:    []byte(infohash),
		InfohashHex: hex.EncodeToString([]byte(infohash)),
		Peer:        &net.TCPAddr{IP: from.IP, Port: int(port)},
	}
}

func (g *GoDHT) send(dict map[string]interface{}, to *net.UDPAddr) error {
	g.conn.WriteTo(bencode.Encode(dict), to)
	return nil
}

func (g *GoDHT) makefriends() {
	for {
		node := <-g.node
		g.findNode(node.addr, []byte(node.id))
	}
}

func (g *GoDHT) genToken(from *net.UDPAddr) string {
	sha1 := sha1.New()
	sha1.Write(from.IP)
	sha1.Write([]byte(g.secret))
	return string(sha1.Sum(nil))
}

func (g *GoDHT) validateToken(token string, from *net.UDPAddr) bool {
	return token == g.genToken(from)
}
