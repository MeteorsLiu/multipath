package main

import (
	"bytes"
	"flag"
	"fmt"
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
			"remoteAddr": "192.168.167.155:29997",
			"weight": 1
		},{
			"remoteAddr": "192.168.167.155:29998",
			"weight": 1
		},{
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

var buf []byte
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

			tBuf := make([]byte, 1024*1024)
			_, err := io.ReadFull(conn, tBuf)
			if err != nil {
				printInfoAndExit("%v", err)
			}
			if !bytes.Equal(tBuf, buf) {
				printInfoAndExit("unexpected buf: want %v got %v", buf, tBuf)
			}
			fmt.Println("done, sending it back")
			// sent it back
			if n, err := conn.Write(tBuf); err != nil || n != 1024*1024 {
				printInfoAndExit("unexpected sending: %v", err)
			}
		}(c)
	}
}

func mockClient(c net.Conn) {
	fmt.Println("start sending segments")

	const bufSizeHalf = 1024 * 1024 / 2
	_, err := c.Write(buf[0:bufSizeHalf])
	if err != nil {
		printInfoAndExit("%v", err)
	}
	_, err = c.Write(buf[bufSizeHalf : bufSizeHalf+bufSizeHalf])
	if err != nil {
		printInfoAndExit("%v", err)
	}
	_, err = c.Write(buf[bufSizeHalf+bufSizeHalf:])
	if err != nil {
		printInfoAndExit("%v", err)
	}

	rBuf := make([]byte, 1024*1024)
	_, err = io.ReadFull(c, rBuf)
	if err != nil {
		printInfoAndExit("%v", err)
	}
	if !bytes.Equal(rBuf, buf) {
		printInfoAndExit("unexpected buf: want %v got %v", buf, rBuf)
	}
	fmt.Println("client done")
}

// mockClientOutOfOrder 模拟乱序发送数据包
// 这个函数模拟在多路径传输中数据包到达顺序混乱的情况
func mockClientOutOfOrder(c net.Conn) {
	fmt.Println("start sending segments out of order")

	// 分成3个大段，每段再分成小片段，模拟多路径传输中的乱序
	const bufSizeThird = 1024 * 1024 / 3

	// 第一段：正常发送
	segment1 := buf[0:bufSizeThird]

	// 第二段：分片乱序发送
	segment2Start := bufSizeThird
	segment2End := bufSizeThird * 2
	segment2 := buf[segment2Start:segment2End]

	// 第三段：正常发送
	segment3 := buf[bufSizeThird*2:]

	fmt.Println("Sending segment 1 (normal order)")
	_, err := c.Write(segment1)
	if err != nil {
		printInfoAndExit("%v", err)
	}

	fmt.Println("Sending segment 2 in fragments with delays (simulating out-of-order)")

	// 将第二段分成4个小片段，乱序发送
	fragmentSize := len(segment2) / 4
	fragments := make([][]byte, 4)

	for i := 0; i < 4; i++ {
		start := i * fragmentSize
		end := start + fragmentSize
		if i == 3 { // 最后一个片段包含剩余的所有数据
			end = len(segment2)
		}
		fragments[i] = segment2[start:end]
	}

	// 乱序发送：3, 1, 4, 2 的顺序
	sendOrder := []int{2, 0, 3, 1} // 对应fragments的索引
	delays := []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond}

	type fragmentTask struct {
		index int
		data  []byte
		delay time.Duration
	}

	tasks := make([]fragmentTask, 4)
	for i, fragIndex := range sendOrder {
		tasks[i] = fragmentTask{
			index: fragIndex,
			data:  fragments[fragIndex],
			delay: delays[i],
		}
	}

	// 并发发送片段，模拟网络延迟导致的乱序
	done := make(chan error, 4)

	for _, task := range tasks {
		go func(t fragmentTask) {
			time.Sleep(t.delay)
			fmt.Printf("Sending fragment %d (size: %d bytes) after %v delay\n",
				t.index+1, len(t.data), t.delay)
			_, err := c.Write(t.data)
			done <- err
		}(task)
	}

	// 等待所有片段发送完成
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			printInfoAndExit("fragment send error: %v", err)
		}
	}

	fmt.Println("Sending segment 3 (normal order)")
	_, err = c.Write(segment3)
	if err != nil {
		printInfoAndExit("%v", err)
	}

	fmt.Println("All segments sent, waiting for response")

	// 接收响应
	rBuf := make([]byte, 1024*1024)
	_, err = io.ReadFull(c, rBuf)
	if err != nil {
		printInfoAndExit("%v", err)
	}
	if !bytes.Equal(rBuf, buf) {
		printInfoAndExit("unexpected buf: want %v got %v", buf, rBuf)
	}
	fmt.Println("client done (out of order test)")
}

func setupForwarder() {
	execCommand("iptables",
		"-t", "nat",
		"-A", "PREROUTING",
		"-p", "udp",
		"--dport", "29997",
		"-j", "DNAT",
		"--to-destination", "127.0.0.1:29999")

	execCommand("iptables",
		"-t", "nat",
		"-A", "PREROUTING",
		"-p", "udp",
		"--dport", "29998",
		"-j", "DNAT",
		"--to-destination", "127.0.0.1:29999")

	execCommand("iptables", "-t", "nat", "-A", "POSTROUTING", "-j", "MASQUERADE")
}

func delForwarder() {
	execCommand("iptables", "-t", "nat", "-nvL")
	execCommand("iptables",
		"-t", "nat",
		"-D", "PREROUTING",
		"-p", "udp",
		"--dport", "29997",
		"-j", "DNAT",
		"--to-destination", "127.0.0.1:29999")

	execCommand("iptables",
		"-t", "nat",
		"-D", "PREROUTING",
		"-p", "udp",
		"--dport", "29998",
		"-j", "DNAT",
		"--to-destination", "127.0.0.1:29999")

	execCommand("iptables", "-t", "nat", "-D", "POSTROUTING", "-j", "MASQUERADE")
}

func main() {
	var testServer bool
	var outOfOrder bool
	flag.BoolVar(&testServer, "server", false, "Test server")
	flag.BoolVar(&outOfOrder, "out-of-order", false, "Test out of order sending")
	flag.Parse()

	buf, _ = os.ReadFile("mockdata.bin")

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
		setupForwarder()
		execCommand("ip", "a", "add", "10.168.168.1", "peer", "10.168.168.2", "dev", "multipath-veth0")
		defer delForwarder()
	} else {
		execCommand("ip", "a", "add", "10.168.168.2", "peer", "10.168.168.1", "dev", "multipath-veth0")
	}
	defer execCommand("ip", "link", "del", "multipath-veth0")

	if testServer {
		l, err := net.Listen("tcp", "10.168.168.1:9999")
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

		if outOfOrder {
			go mockClientOutOfOrder(c)
		} else {
			go mockClient(c)
		}
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
