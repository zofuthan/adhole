// See LICENSE.txt for licensing information.

package main

import (
	"bufio"
	"bytes"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// query wraps Host name and clients UDPAddr.
type query struct {
	Host string
	From *net.UDPAddr
}

// String prints human-readable representation of a query.
func (q *query) String() string {
	return fmt.Sprintf("from %s about %s", q.From, q.Host)
}

// Flags.
var (
	flagVerbose  = flag.Bool("v", false, "be verbose")
	flagHTTPPort = flag.Int("hport", 80, "HTTP server port")
	flagDNSPort  = flag.Int("dport", 53, "DNS server port")
	flagTimeout  = flag.Duration("t", 5*time.Second, "upstream query timeout")
)

// Expvar exported statistics counters.
var (
	cntMsgs     = expvar.NewInt("statsQuestions")
	cntRelayed  = expvar.NewInt("statsRelayed")
	cntBlocked  = expvar.NewInt("statsBlocked")
	cntTimedout = expvar.NewInt("statsTimedout")
	cntServed   = expvar.NewInt("statsServed")
	cntErrors   = expvar.NewInt("statsErrors")
	cntRules    = expvar.NewInt("statsRules")
)

// 'Static' variables.
// answer will become static once the proxy address bytes have been added.
var (
	// answer is the header of a DNS query response without the domain name and
	// the resource data.
	//
	// Good sources of information on the DNS protocol can be found at:
	// http://www.firewall.cx/networking-topics/protocols/domain-name-system-dns
	// http://www.iana.org/assignments/dns-parameters/dns-parameters.xhtml
	//
	// Bytes described:
	// 2 - Type        = 0x0001     - A
	// 2 - Class       = 0x0001     - IN
	// 4 - TTL         = 0xffffffff - if anyone respects it, this should reduce hits
	// 2 - Data Length = 0x0004     - number of resource bytes, which for IN A IPv4 address is exactly 4
	answer = []byte("\x00\x01\x00\x01\xff\xff\xff\xff\x00\x04")

	// pixel is a hex representation of an 'empty' 1x1 GIF image.
	pixel = "\x47\x49\x46\x38\x39\x61\x01\x00\x01\x00\x80\x00\x00\xff\xff" +
		"\xff\x00\x00\x00\x21\xf9\x04\x01\x00\x00\x00\x00\x2c\x00\x00" +
		"\x00\x00\x01\x00\x01\x00\x00\x02\x02\x44\x01\x00\x3b"
)

var (
	proxy    *net.UDPConn
	upstream *net.UDPConn
	queries  map[int]*query
	blocked  map[string]bool
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] upstream proxy list.txt\n\n"+
			"upstream - real upstream DNS address, e.g. 8.8.8.8\n"+
			"proxy    - servers' bind address, e.g. 127.0.0.1\n"+
			"list.txt - text file with domains to block\n\n",
			os.Args[0],
		)
		flag.PrintDefaults()
		return
	}
	flag.Parse()

	if len(flag.Args()) < 3 {
		flag.Usage()
		os.Exit(1)
	}

	upIP := net.ParseIP(flag.Arg(0))
	if upIP == nil {
		fmt.Fprintf(os.Stderr, "ERROR: Can't parse upstream IP '%s'\n", flag.Arg(0))
		os.Exit(2)
	}

	upIP = upIP.To4()
	if upIP == nil {
		fmt.Fprintln(os.Stderr, "ERROR: IPv6 is not supported, sorry")
		os.Exit(3)
	}

	proxyIP := net.ParseIP(flag.Arg(1))
	if proxyIP == nil {
		fmt.Fprintf(os.Stderr, "ERROR: Can't parse proxy IP '%s'\n", flag.Arg(1))
		os.Exit(2)
	}

	proxyIP = proxyIP.To4()
	if proxyIP == nil {
		fmt.Fprintln(os.Stderr, "ERROR: IPv6 is not supported, sorry")
		os.Exit(3)
	}
	answer = append(answer, proxyIP...)

	parseList(flag.Arg(2))

	var err error
	upAddr := &net.UDPAddr{IP: upIP, Port: 53}
	upstream, err = net.DialUDP("udp4", nil, upAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(2)
	}
	defer upstream.Close()

	proxyAddr := &net.UDPAddr{IP: proxyIP, Port: *flagDNSPort}
	proxy, err = net.ListenUDP("udp4", proxyAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(2)
	}
	defer proxy.Close()

	queries = make(map[int]*query, 4096)

	go runServerHTTP(flag.Arg(1))
	go runServerUpstreamDNS()
	go runServerLocalDNS()

	sigloop()
}

// parseList loads a block list file into blocked and updates rules counter.
func parseList(path string) {
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
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
	cntRules.Set(int64(counter))

	return
}

// runServerLocalDNS listens for incoming DNS queries and dispatches them for processing.
func runServerLocalDNS() {
	log.Println("DNS: Started local server at", proxy.LocalAddr())

	buf := make([]byte, 512)
	for {
		n, addr, err := proxy.ReadFromUDP(buf)
		if err != nil {
			log.Println("DNS ERROR (1):", err)
			cntErrors.Add(1)
			continue
		}

		msg := make([]byte, n)
		copy(msg, buf[:n])
		cntMsgs.Add(1)
		go handleDNS(msg, addr)
	}
}

// runServerUpstreamDNS listens for upstream answers and relies them to original clients.
func runServerUpstreamDNS() {
	log.Println("DNS: Started upstream server")

	buf := make([]byte, 512)
	for {
		n, _, err := upstream.ReadFromUDP(buf)
		if err != nil {
			log.Println("DNS ERROR (2):", err)
			cntErrors.Add(1)
			continue
		}

		id := int(uint16(buf[0])<<8 + uint16(buf[1]))
		if query, ok := queries[id]; ok {
			delete(queries, id)
			_, err := proxy.WriteTo(buf[:n], query.From)
			if err != nil {
				log.Printf("DNS ERROR: Query id %d %s %s", id, query, err)
				cntErrors.Add(1)
				continue
			}
			if *flagVerbose {
				log.Println("DNS: Relayed answer to query", id)
			}
			cntRelayed.Add(1)
		}
	}
}

// handleDNS peeks the query and either relies it to the upstream DNS server or returns
// a static answer with the 'fake' IP.
func handleDNS(msg []byte, from *net.UDPAddr) {
	var domain bytes.Buffer
	var block bool

	id := int(uint16(msg[0])<<8 + uint16(msg[1]))
	if *flagVerbose {
		log.Printf("DNS: Query id %d from %s\n", id, from)
	}

	count := uint8(msg[5]) // question counter
	offset := 12           // point to first domain name

	if count != 1 {
		log.Printf("DNS WARN: Query id %d from %s has %d questions\n", id, from, count)
		return
	}

	for {
		length := int8(msg[offset])
		if length == 0 {
			break
		}
		offset++
		domain.WriteString(string(msg[offset : offset+int(length)]))
		domain.WriteString(".")
		offset += int(length)
	}
	host := domain.String()
	testHost := host
	parts := strings.Split(testHost, ".")
	try := 1
	for {
		if _, ok := blocked[testHost]; ok {
			block = true
			break
		}
		parts = parts[1:]
		if len(parts) < 3 {
			break
		}
		testHost = strings.Join(parts, ".")
		try++
	}

	if block {
		if *flagVerbose {
			log.Printf("DNS: Blocking (%d) %s\n", try, host)
		}
		cntBlocked.Add(1)

		msg[2] = uint8(129) // flags upper byte
		msg[3] = uint8(128) // flags lower byte
		msg[7] = uint8(1)   // answer counter

		msg = append(msg, msg[12:12+1+len(host)]...) // domain
		msg = append(msg, answer...)                 // payload
		_, err := proxy.WriteTo(msg, from)
		if err != nil {
			log.Println("DNS ERROR (3):", err)
			cntErrors.Add(1)
			return
		}
		if *flagVerbose {
			log.Println("DNS: Sent fake answer")
		}
	} else {
		if *flagVerbose {
			log.Println("DNS: Asking upstream")
		}
		queries[id] = &query{From: from, Host: host}
		_, err := upstream.Write(msg)
		if err != nil {
			log.Println("DNS ERROR (4):", err)
			cntErrors.Add(1)
			delete(queries, id)
			return
		}
		go func(queryID int) {
			time.Sleep(*flagTimeout)
			if query, ok := queries[queryID]; ok {
				fmt.Printf("DNS WARN: Query id %d %s timed out\n", queryID, query)
				cntTimedout.Add(1)
				delete(queries, queryID)
			}
			return
		}(id)
	}

	return
}

// handleHTTP returns an 'empty' 1x1 GIF image for any URL.
func handleHTTP(w http.ResponseWriter, req *http.Request) {
	if *flagVerbose {
		log.Printf("HTTP: Request %s %s %s\n", req.Method, req.Host, req.RequestURI)
	}
	cntServed.Add(1)
	w.Header()["Content-type"] = []string{"image/gif"}
	io.WriteString(w, pixel)

	return
}

// runServerHTTP starts the HTTP server.
func runServerHTTP(host string) {
	addr := fmt.Sprintf("%s:%d", host, *flagHTTPPort)
	http.HandleFunc("/", handleHTTP)
	log.Println("HTTP: Started at", addr)
	log.Fatalln(http.ListenAndServe(addr, nil))

	panic("not reachable")
}

// vim: ts=4 sw=4 sts=4
