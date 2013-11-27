package main

import (
	"github.com/badgerodon/proxy"
	"log"
)

func main() {
	log.SetFlags(log.Lshortfile)

	proxy, err := proxy.NewProxy("config.json")
	if err != nil {
		log.Fatalln(err)
	}
	defer proxy.Close()
	err = proxy.ListenAndServe()
	if err != nil {
		log.Fatalln(err)
	}
}
