package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"xagent/internal/testutil"
)

func main() {
	mode := flag.String("mode", string(testutil.FakeProviderSuccess), "success|block|http_401|tool_bash_fail")
	flag.Parse()

	fake := testutil.NewFakeProviderServer(testutil.FakeProviderMode(*mode))
	defer fake.Close()

	url := fake.URL()
	if host, port, err := net.SplitHostPort(fake.Server.Listener.Addr().String()); err == nil {
		if host == "" || host == "::" {
			host = "127.0.0.1"
		}
		url = "http://" + net.JoinHostPort(host, port)
	}
	fmt.Println(url)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("fake provider stopped")
}

var _ http.Handler
