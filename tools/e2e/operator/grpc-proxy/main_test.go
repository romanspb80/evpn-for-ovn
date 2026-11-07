package main

import (
	"bytes"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestByteStreamTransport(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "provider.sock")
	provider, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	request := bytes.Repeat([]byte{0, 1, 2, 255}, 16384)
	response := bytes.Repeat([]byte{255, 2, 1, 0}, 16384)
	providerResult := make(chan error, 1)
	go func() {
		connection, err := provider.Accept()
		if err != nil {
			providerResult <- err
			return
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			providerResult <- err
			return
		}
		got, err := io.ReadAll(connection)
		if err != nil {
			providerResult <- err
			return
		}
		if !bytes.Equal(got, request) {
			providerResult <- io.ErrUnexpectedEOF
			return
		}
		_, err = connection.Write(response)
		providerResult <- err
	}()
	client, done := proxyClient(t, socket)
	defer client.Close()
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(client)
	if err != nil || !bytes.Equal(got, response) {
		t.Fatalf("transport corrupted response: length=%d err=%v", len(got), err)
	}
	if err := <-providerResult; err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy leaked a connection goroutine")
	}
}

func TestUnavailableProviderClosesClient(t *testing.T) {
	client, done := proxyClient(t, filepath.Join(t.TempDir(), "missing.sock"))
	defer client.Close()
	data, err := io.ReadAll(client)
	if err != nil || len(data) != 0 {
		t.Fatalf("failed provider returned success-shaped data: %q %v", data, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("unavailable provider leaked connection")
	}
}

func proxyClient(t *testing.T, socket string) (*net.TCPConn, <-chan struct{}) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan struct{})
	go func() {
		connection, err := listener.AcceptTCP()
		if err != nil {
			close(done)
			return
		}
		forward(connection, socket)
		close(done)
	}()
	connection, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return connection, done
}
