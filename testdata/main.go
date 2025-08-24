package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const (
	cfgFileClient = `{
	"client": {
		"remotePaths": [{
			"remoteAddr": "192.168.167.155:29999",
			"weight": 1
		}]
	}
}`
	cfgFileServer = `{
	"server": {
		"listen": "0.0.0.0:29999"
	},
	"isServer": true
}`
)

var buf = make([]byte, 16384)
var sigCh = make(chan os.Signal, 1)

func printInfoAndExit(f string, err ...any) {
	log.Printf(f, err...)
	sigCh <- os.Kill
}

func mockServer(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()

			tBuf := make([]byte, 16384)
			_, err := io.ReadFull(conn, tBuf)
			if err != nil {
				printInfoAndExit("%v", err)
			}
			if !bytes.Equal(tBuf, buf) {
				printInfoAndExit("unexpected buf: want %v got %v", buf, tBuf)
			} else {
				printInfoAndExit("verify done")
			}
		}(c)
	}
}

func mockClient(c net.Conn) {
	_, err := c.Write(buf)
	if err != nil {
		printInfoAndExit("%v", err)
	}
}

func main() {
	var testServer bool
	flag.BoolVar(&testServer, "server", false, "Test server")
	flag.Parse()

	io.ReadFull(rand.Reader, buf)

	tempConfig, err := os.CreateTemp("", "mock-test*.cfg")
	if err != nil {
		panic(err)
	}
	defer os.Remove(tempConfig.Name())

	if testServer {
		tempConfig.Write([]byte(cfgFileServer))
	} else {
		tempConfig.Write([]byte(cfgFileClient))
	}

	cmd := exec.Command("go", "run", "..", "-config", tempConfig.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	defer cmd.Process.Kill()

	go cmd.Run()

	// wait process start
	time.Sleep(2 * time.Second)

	execCommand("ip", "link", "set", "multipath-veth0", "up")
	if testServer {
		execCommand("ip", "a", "add", "10.168.168.1", "peer", "10.168.168.2", "dev", "multipath-veth0")
	} else {
		execCommand("ip", "a", "add", "10.168.168.2", "peer", "10.168.168.1", "dev", "multipath-veth0")
	}
	defer execCommand("ip", "link", "del", "multipath-veth0")

	if testServer {
		l, err := net.Listen("tcp", "0.0.0.0:9999")
		if err != nil {
			panic(err)
		}
		defer l.Close()

		go mockServer(l)
	} else {
		time.Sleep(2 * time.Second)

		c, err := net.Dial("tcp", "10.168.168.1:9999")
		if err != nil {
			panic(err)
		}
		defer c.Close()

		go mockClient(c)
	}

	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	<-sigCh
}

func execCommand(cmd string, args ...string) {
	current := exec.Command(cmd, args...)
	current.Stdout = os.Stdout
	current.Stderr = os.Stderr
	current.Run()
}
