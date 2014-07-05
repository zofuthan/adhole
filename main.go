package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
)

var (
	pixel = "\x47\x49\x46\x38\x39\x61\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00\x21\xf9\x04\x01\x00\x00\x00\x00\x2c\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02\x44\x01\x00\x3b"
	exts  = map[string]bool{
		"jpg":  true,
		"jpeg": true,
		"png":  true,
		"gif":  true,
	}
	proxy    *net.UDPConn
	upstream *net.UDPConn
	blocked  map[string]bool
	answer   []byte
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s upstream proxy list.txt\n\n"+
			"   upstream - real upstream DNS address, e.g. 8.8.8.8\n"+
			"   proxy    - your address, e.g. 127.0.0.1\n"+
			"   list.txt - text file with addresses to kill\n",
			os.Args[0],
		)
		os.Exit(1)
	}

	upIP := net.ParseIP(os.Args[1])
	if upIP == nil {
		fmt.Fprintf(os.Stderr, "ERROR: Can't parse upstream IP '%s'\n", os.Args[1])
		os.Exit(2)
	}

	proxyIP := net.ParseIP(os.Args[2])
	if proxyIP == nil {
		fmt.Fprintf(os.Stderr, "ERROR: Can't parse proxy IP '%s'\n", os.Args[2])
		os.Exit(2)
	}

	answer = []byte("\x00\x01\x00\x01\xff\xff\xff\xff\x00\x04")
	answerIP := proxyIP.To4()
	if answerIP == nil {
		fmt.Fprintln(os.Stderr, "ERROR: IPv6 is not supported, sorry")
		os.Exit(3)
	}
	answer = append(answer, answerIP...)

	parseList(os.Args[3])

	var err error
	upAddr := &net.UDPAddr{IP: upIP, Port: 53}
	upstream, err = net.DialUDP("udp", nil, upAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(2)
	}
	defer upstream.Close()

	proxyAddr := &net.UDPAddr{IP: proxyIP, Port: 53}
	proxy, err = net.ListenUDP("udp", proxyAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(2)
	}
	defer proxy.Close()

	go runServerNilData(os.Args[2])
	go runServerProxy()

	sig := make(chan os.Signal)
	signal.Notify(sig, os.Interrupt)

forever:
	for {
		select {
		case <-sig:
			log.Println("Signal received, stopping")
			break forever
		}
	}
}

func parseList(path string) {
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR:", err)
		os.Exit(2)
	}
	defer file.Close()

	blocked = make(map[string]bool, 4096)
	counter := 0
	scn := bufio.NewScanner(file)
	for scn.Scan() {
		counter++
		blocked[scn.Text()+"."] = true
	}

	log.Printf("DNS: Parsed %d entries from list\n", counter)
	return
}

func runServerProxy() {
	log.Println("DNS: Started server at", proxy.LocalAddr())

	buf := make([]byte, 65536)
	oobuf := make([]byte, 512)

	for {
		n, _, _, addr, err := proxy.ReadMsgUDP(buf, oobuf)
		if err != nil {
			log.Println("DNS ERROR:", err)
			continue
		}

		if n < 12 {
			log.Println("DNS ERROR: Query length:", n)
		} else {
			go handleDNS(buf[:n], addr)
		}
	}

	panic("not reachable (1)")
}

func popPart(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 3 {
		return ""
	}

	return strings.Join(parts[1:], ".")
}

func handleDNS(msg []byte, from *net.UDPAddr) {
	var domain bytes.Buffer
	var host string
	var block bool
	var try int

	//log.Println("DNS: Query from", from)

	// peak query
	count := uint16(msg[5]) // question counter
	offset := uint16(12)    // point to first domain name

	// TODO(drbig): Will this be a problem IRL?
	if count != 1 {
		log.Fatalln("DNS: Question counter =", count)
		os.Exit(127)
	}

outer:
	for count > 0 {
	inner:
		for {
			length := int8(msg[offset])
			if length == 0 {
				break inner
			}
			offset++
			domain.WriteString(string(msg[offset : offset+uint16(length)]))
			domain.WriteString(".")
			offset += uint16(length)
		}
		host = domain.String()
		testHost := host
		try = 1

	test:
		for {
			if _, ok := blocked[testHost]; ok {
				block = true
				break outer
			}
			testHost = popPart(testHost)
			if testHost == "" {
				break test
			}
			try++
		}
		domain.Reset()

		offset += 4
		count--
	}
	// end peak query

	if block {
		// fake answer
		log.Printf("DNS: Blocking (%d) %s\n", try, host)

		msg[2] = uint8(129) // flags upper byte
		msg[3] = uint8(128) // flags lower byte
		msg[7] = uint8(1)   // answer counter

		res := append(msg, msg[12:12+1+len(host)]...)
		res = append(res, answer...)
		n, err := proxy.WriteTo(res, from)
		if err != nil {
			log.Println("DNS ERROR:", err)
			return
		}
		if n != len(res) {
			log.Println("DNS ERROR: Length mismatch")
			return
		}

		log.Println("DNS: Sent fake answer")
		return
		// end fake answer
	} else {
		//log.Println("DNS: Asking upstream")
		n, err := upstream.Write(msg)
		if err != nil {
			log.Println("DNS ERROR:", err)
			return
		}
		if n != len(msg) {
			log.Println("DNS ERROR: Length mismatch")
			return
		}

		buf := make([]byte, 65536)
		oobuf := make([]byte, 512)
		n, _, _, _, err = upstream.ReadMsgUDP(buf, oobuf)
		if err != nil {
			log.Println("DNS ERROR:", err)
			return
		}

		sn, err := proxy.WriteTo(buf[:n], from)
		if err != nil {
			log.Println("DNS ERROR:", err)
			return
		}
		if sn != n {
			log.Println("DNS ERROR: Length mismatch")
			return
		}

		//log.Println("DNS: Relayed answer")
		return
	}

	panic("not reachable (2)")
}

func nilHandler(w http.ResponseWriter, req *http.Request) {
	log.Printf("HTTP: Request %s %s\n", req.Method, req.URL)
	parts := strings.Split(req.URL.Path, ".")
	ext := parts[len(parts)-1]
	if _, ok := exts[ext]; ok {
		//log.Println("HTTP: Sending image")
		w.Header()["Content-type"] = []string{"image/gif"}
		io.WriteString(w, pixel)
	} else {
		//log.Println("HTTP: Sending string")
		io.WriteString(w, "nil\n")
	}
}

func runServerNilData(host string) {
	addr := fmt.Sprintf("%s:80", host)
	http.HandleFunc("/", nilHandler)
	log.Println("HTTP: Started at", addr)
	log.Fatalln(http.ListenAndServe(addr, nil))
}

// vim: ts=4 sw=4 sts=4