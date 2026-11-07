// Test-only byte-stream transport for the independent Unix-only BGP provider.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"time"
)

func main() {
	listen := flag.String("listen", ":50054", "TCP listener")
	socket := flag.String("socket", "/var/run/gobgp/gobgp.sock", "provider Unix socket")
	flag.Parse()
	server, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	for {
		connection, err := server.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go forward(connection, *socket)
	}
}

func forward(connection net.Conn, socket string) {
	defer connection.Close()
	upstream, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		log.Printf("connect provider: %v", err)
		return
	}
	defer upstream.Close()
	done := make(chan struct{})
	go func() {
		if _, err := io.Copy(upstream, connection); err != nil {
			log.Printf("forward request: %v", err)
		}
		if err := upstream.(*net.UnixConn).CloseWrite(); err != nil {
			log.Printf("half-close provider: %v", err)
		}
		close(done)
	}()
	if _, err := io.Copy(connection, upstream); err != nil {
		log.Printf("forward response: %v", err)
	}
	if err := connection.(*net.TCPConn).CloseWrite(); err != nil {
		log.Printf("half-close client: %v", err)
	}
	if err := connection.(*net.TCPConn).CloseRead(); err != nil {
		log.Printf("close client reader: %v", err)
	}
	<-done
}
