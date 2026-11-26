// Command fakejf serves the in-memory fake Jellyfin so jterm can be driven
// end to end without a real server:
//
//	go run ./internal/jftest/fakejf -video some.mkv
//	JTERM_CONFIG_DIR=$(mktemp -d) JTERM_DISCOVERY_ADDR=127.0.0.1:17359 jterm
//
// Sign in as demo / demo.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"github.com/h0bnobs/jterm/internal/jftest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18096", "HTTP listen address")
	disco := flag.String("discovery", "127.0.0.1:17359", "UDP discovery listen address")
	video := flag.String("video", "", "file to serve for every stream")
	failTranscode := flag.Bool("fail-transcode", false, "refuse transcode requests")
	flag.Parse()

	srv := jftest.New()
	srv.VideoPath = *video
	srv.FailTranscode = *failTranscode

	udpAddr, err := net.ResolveUDPAddr("udp4", *disco)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		log.Fatal(err)
	}
	go jftest.ServeDiscovery(conn, "http://"+*addr)

	log.Printf("fake Jellyfin on http://%s (discovery on %s), sign in as %s / %s",
		*addr, *disco, jftest.Username, jftest.Password)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
