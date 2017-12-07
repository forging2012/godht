package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/fanpei91/bencode"
)

const (
	secret = "IYHJFR%^&IO"
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
type announce struct {
	announce map[string]interface{}
	from     *net.UDPAddr
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

func genToken(from *net.UDPAddr) string {
	sha1 := sha1.New()
	sha1.Write(from.IP)
	sha1.Write([]byte(secret))
	return string(sha1.Sum(nil))
}

func validateToken(token string, from *net.UDPAddr) bool {
	return token == genToken(from)
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

type godht struct {
	announce       chan *announce
	node           chan *node
	localID        nodeID
	conn           *net.UDPConn
	queryTypes     map[string]func(map[string]interface{}, *net.UDPAddr)
	friendsLimiter *rate.Limiter
}

func newDHT(laddr string, maxFriendsPerSec int) (*godht, error) {
	conn, err := net.ListenPacket("udp4", laddr)
	if err != nil {
		return nil, err
	}
	localID := randBytes(20)
	g := &godht{
		localID:        localID,
		announce:       make(chan *announce),
		conn:           conn.(*net.UDPConn),
		node:           make(chan *node),
		friendsLimiter: rate.NewLimiter(per(maxFriendsPerSec, time.Second), maxFriendsPerSec),
	}
	g.queryTypes = map[string]func(map[string]interface{}, *net.UDPAddr){
		"ping":          g.onPingQuery,
		"find_node":     g.onFindNodeQuery,
		"get_peers":     g.onGetPeersQuery,
		"announce_peer": g.onAnnouncePeerQuery,
	}
	go g.listen()
	go g.join()
	go g.makefriends()
	return g, nil
}

func (g *godht) listen() error {
	buf := make([]byte, 8192)
	for {
		n, addr, err := g.conn.ReadFromUDP(buf)
		if err == nil {
			go g.onMessage(buf[:n], addr)
		}
	}
}

func (g *godht) onMessage(data []byte, from *net.UDPAddr) {
	dict, err := bencode.Decode(bytes.NewBuffer(data))
	if err != nil {
		return
	}
	tid, ok := dict["t"].(string)
	if !ok {
		g.error(tid, from)
		return
	}
	y, ok := dict["y"].(string)
	if !ok {
		g.error(tid, from)
		return
	}
	switch y {
	case "q":
		g.onQuery(dict, from)
	case "r", "e":
		g.onReply(dict, from)
	}
}

func (g *godht) onQuery(dict map[string]interface{}, from *net.UDPAddr) {
	tid, ok := dict["t"].(string)
	if !ok {
		return
	}
	q, ok := dict["q"].(string)
	if !ok {
		g.error(tid, from)
		return
	}
	if f, ok := g.queryTypes[q]; ok {
		f(dict, from)
	}
}

func (g *godht) onReply(dict map[string]interface{}, from *net.UDPAddr) {
	r, ok := dict["r"].(map[string]interface{})
	if !ok {
		return
	}
	nodes, ok := r["nodes"].(string)
	if !ok {
		return
	}
	for _, node := range decodeNodes(nodes) {
		g.node <- node
	}
}

func (g *godht) findNode(to string, target nodeID) {
	d := makeQuery(string(randBytes(4)), "find_node", map[string]interface{}{
		"id":     string(neighborID(target, g.localID)),
		"target": string(target),
	})
	addr, err := net.ResolveUDPAddr("udp4", to)
	if err != nil {
		return
	}
	g.send(d, addr)
}

func (g *godht) error(tid string, to *net.UDPAddr) {
	e := map[string]interface{}{
		"t": tid,
		"y": "e",
		"e": []interface{}{
			203, "Protocol Error Ocurred",
		},
	}
	g.send(e, to)
}

func (g *godht) onPingQuery(dict map[string]interface{}, from *net.UDPAddr) {
	t := dict["t"].(string)
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		g.error(t, from)
		return
	}
	id, ok := a["id"].(string)
	if !ok {
		g.error(t, from)
		return
	}
	d := makeReply(t, map[string]interface{}{
		"id": string(neighborID([]byte(id), g.localID)),
	})
	g.send(d, from)
}

func (g *godht) onFindNodeQuery(dict map[string]interface{}, from *net.UDPAddr) {
	t := dict["t"].(string)
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		g.error(t, from)
		return
	}
	id, ok := a["id"].(string)
	if !ok {
		g.error(t, from)
		return
	}
	d := makeReply(t, map[string]interface{}{
		"id":    string(neighborID([]byte(id), g.localID)),
		"nodes": "",
	})
	g.send(d, from)
}

func (g *godht) onGetPeersQuery(dict map[string]interface{}, from *net.UDPAddr) {
	t := dict["t"].(string)
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		g.error(t, from)
		return
	}
	id, ok := a["id"].(string)
	if !ok {
		g.error(t, from)
		return
	}
	d := makeReply(t, map[string]interface{}{
		"id":    string(neighborID([]byte(id), g.localID)),
		"nodes": "",
		"token": genToken(from),
	})
	g.send(d, from)
}

func (g *godht) onAnnouncePeerQuery(dict map[string]interface{}, from *net.UDPAddr) {
	t := dict["t"].(string)
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		g.error(t, from)
		return
	}
	id, ok := a["id"].(string)
	if !ok {
		g.error(t, from)
		return
	}
	token, ok := a["token"].(string)
	if !ok || !validateToken(token, from) {
		g.error(t, from)
		return
	}
	d := makeReply(t, map[string]interface{}{
		"id": string(neighborID([]byte(id), g.localID)),
	})
	g.send(d, from)
	g.announce <- &announce{announce: dict, from: from}
}

func (g *godht) send(dict map[string]interface{}, to *net.UDPAddr) error {
	g.conn.WriteTo(bencode.Encode(dict), to)
	return nil
}

func (g *godht) join() {
	for range time.Tick(3 * time.Second) {
		for _, boot := range BootstrapNodes {
			g.findNode(boot, randBytes(20))
		}
	}
}

func (g *godht) makefriends() {
	for {
		if err := g.friendsLimiter.Wait(context.Background()); err != nil {
			continue
		}
		node := <-g.node
		g.findNode(node.addr, []byte(node.id))
	}
}

func magnetLink(announce map[string]interface{}) string {
	if a, ok := announce["a"].(map[string]interface{}); ok {
		infohash := a["info_hash"].(string)
		return fmt.Sprintf("magnet:?xt=urn:btih:%v", hex.EncodeToString([]byte(infohash)))
	}
	return ""
}

func main() {
	laddr, maxFriendsPerSec := os.Args[1], os.Args[2]
	max, err := strconv.Atoi(maxFriendsPerSec)
	if err != nil {
		panic(err)
	}
	dht, err := newDHT(laddr, max)
	if err != nil {
		panic(err)
	}
	for announce := range dht.announce {
		fmt.Println(magnetLink(announce.announce))
	}
}
