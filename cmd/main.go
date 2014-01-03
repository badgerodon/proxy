package main

import (
	"github.com/badgerodon/proxy"
	"log"
)

func main() {
	log.SetFlags(log.Lshortfile)

	proxy.Listen("config.json")
}
