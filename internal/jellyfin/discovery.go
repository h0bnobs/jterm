package jellyfin

import (
	"encoding/json"
	"net"
	"os"
	"time"
)

// Jellyfin's discovery protocol: broadcast this datagram on port 7359 and
// every server on the network answers with a line of JSON.
const (
	discoveryPort    = 7359
	discoveryMessage = "who is JellyfinServer?"
	// DiscoveryTimeout is how long to listen for answers.
	DiscoveryTimeout = 2 * time.Second
)

// Discover broadcasts the discovery datagram and collects the servers that
// answer within the timeout, in the order they replied.
//
// JTERM_DISCOVERY_ADDR (host:port) sends the datagram somewhere other than
// the local broadcast address: a directed broadcast for another subnet, or a
// test server on loopback.
func Discover(timeout time.Duration) []ServerInfo {
	target := &net.UDPAddr{IP: net.IPv4bcast, Port: discoveryPort}
	if override := os.Getenv("JTERM_DISCOVERY_ADDR"); override != "" {
		if addr, err := net.ResolveUDPAddr("udp4", override); err == nil {
			target = addr
		}
	}
	return discover(target, timeout)
}

func discover(target *net.UDPAddr, timeout time.Duration) []ServerInfo {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil
	}
	defer conn.Close()
	// Go enables SO_BROADCAST on UDP sockets by default.
	if _, err := conn.WriteToUDP([]byte(discoveryMessage), target); err != nil {
		return nil
	}
	var found []ServerInfo
	seen := map[string]bool{}
	buf := make([]byte, 8192)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return found // deadline reached
		}
		var info ServerInfo
		if json.Unmarshal(buf[:n], &info) != nil || info.Address == "" {
			continue
		}
		key := info.ID
		if key == "" {
			key = info.Address
		}
		if !seen[key] {
			seen[key] = true
			found = append(found, info)
		}
	}
}
