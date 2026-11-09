// ga (ga.fyi) — gateway for agents: @ an AI agent from WeChat and it answers.
//
// A relay that holds the WeChat ClawBot (iLink Bot API) session(s) and routes
// messages to multiple agents: "@name" in WeChat picks the agent, and every
// agent can message you back through its personal link.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

const version = "0.4.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "genkey":
			var b [24]byte
			rand.Read(b[:])
			fmt.Println(hex.EncodeToString(b[:]))
			return
		case "version", "--version", "-v":
			fmt.Println("gafyi", version)
			return
		}
	}
	cfgPath := flag.String("config", "/etc/gafyi/config.json", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	h, err := newHub(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	h.start()

	srv := &http.Server{Addr: cfg.Listen, Handler: h.routes(), ReadHeaderTimeout: 10 * time.Second}
	accs := h.listAccounts()
	n := 0
	for _, a := range accs {
		n += len(a.Agents)
	}
	log.Printf("gafyi %s listening on %s (%d wechat accounts, %d connections)", version, cfg.Listen, len(accs), n)
	log.Fatal(srv.ListenAndServe())
}
