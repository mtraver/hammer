package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	termbox "github.com/nsf/termbox-go"
)

var (
	numWorkers = flag.Int64("w", 1, "number of concurrent workers")
	fetcher    = flag.String("fetcher", "go", "type of fetcher to use: go|noop|curl")
	headers    = make(map[string]string)
	insecure   = flag.Bool("insecure", false, "skip cert verification")
	timeout    = flag.Duration("timeout", time.Minute, "timeout for requests")
)

const interval = time.Second

// This is a histogram of events over the past second.
var hmu sync.Mutex
var histogram = make(map[string]int)

// This is a histogram of latencies in the past second.
// Usually they won't overlap, but that's fine.
var lmu sync.Mutex
var latencies = make(map[time.Duration]int)

type headersFlag struct {
	headers map[string]string
}

func (h headersFlag) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n")
	for k, v := range h.headers {
		fmt.Fprintf(&b, "  %s=%s\n", k, v)
	}
	fmt.Fprintf(&b, "}")
	return b.String()
}

func (h headersFlag) Set(value string) error {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return errors.New("header must be of the form key:value")
	}

	h.headers[parts[0]] = strings.TrimSpace(parts[1])
	return nil
}

func init() {
	flag.Var(&headersFlag{headers}, "header", "HTTP header to include in each request, in key:value format. You may give the flag multiple times to specify multiple headers.")
}

func main() {
	flag.Parse()
	switch *fetcher {
	case "go":
	case "noop":
	case "curl":
	default:
		fmt.Printf("--fetcher set to %q, want one of \"go\", \"noop\", or \"curl\"\n", *fetcher)
		os.Exit(1)
	}

	if flag.NArg() != 1 {
		fmt.Printf("Usage: hammer [flags] url\n")
		os.Exit(0)
	}
	u := flag.Arg(0)
	if !strings.HasPrefix(u, "http") {
		u = "http://" + u
	}

	err := termbox.Init()
	if err != nil {
		panic(err)
	}
	termbox.SetInputMode(termbox.InputEsc | termbox.InputMouse)
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
	draw(u)

	doneChan := make(chan struct{})
	go hammer(u, doneChan)
	go sendTermboxInterrupts()

	for {
		switch ev := termbox.PollEvent(); ev.Type {
		case termbox.EventKey:
			switch ev.Key {
			case termbox.KeyArrowUp:
				// Start some more workers.
				nw2 := 2 * atomic.LoadInt64(numWorkers)
				for atomic.LoadInt64(numWorkers) < nw2 {
					go worker(u, doneChan)
					atomic.StoreInt64(numWorkers, atomic.LoadInt64(numWorkers)+1)
				}
			case termbox.KeyArrowDown:
				// Stop some existing workers.
				nw2 := atomic.LoadInt64(numWorkers) / 2
				if nw2 == 0 {
					nw2 = 1
				}
				for atomic.LoadInt64(numWorkers) > nw2 {
					doneChan <- struct{}{}
					atomic.StoreInt64(numWorkers, atomic.LoadInt64(numWorkers)-1)
				}
			case termbox.KeyCtrlC:
				// Quit
				termbox.Close()
				os.Exit(0)
			}
		case termbox.EventInterrupt:
			draw(u)
		}
	}
}

func hammer(url string, doneChan chan struct{}) {
	// Spin up workers.
	for i := int64(0); i < atomic.LoadInt64(numWorkers); i++ {
		go worker(url, doneChan)
	}
}

func worker(url string, doneChan chan struct{}) {
	for {
		// Quit if the done chan says so.
		select {
		case <-doneChan:
			return
		default:
		}

		// Do some work.
		t0 := time.Now()
		dt := func() time.Duration { return time.Now().Sub(t0) }
		switch *fetcher {
		case "curl":
			flags := []string{"-s", "-S", "-L", "-o", "/dev/null", "-w", "%{http_code}"}
			if *insecure {
				flags = append(flags, "-k")
			}
			for k, v := range headers {
				flags = append(flags, "-H", fmt.Sprintf("%s: %s", k, v))
			}
			flags = append(flags, url)

			cmd := exec.Command("curl", flags...)
			out, _ := cmd.CombinedOutput()

			// Get HTTP status code text if the output is an integer status code,
			// or otherwise use the whole output.
			status := string(out)
			if s, err := strconv.ParseInt(status, 10, 64); err == nil {
				status = fmt.Sprintf("%v %s", s, http.StatusText(int(s)))
			}

			addToHistograms(status, dt())
		case "go":
			client := http.Client{Timeout: *timeout}
			if *insecure {
				client.Transport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}

			req, _ := http.NewRequest(http.MethodGet, url, nil)
			for k, v := range headers {
				req.Header.Add(k, v)
			}

			resp, err := client.Do(req)
			if resp != nil {
				// Read it, just in case that matters somehow.
				if _, err := ioutil.ReadAll(resp.Body); err != nil {
					addToHistograms(fmt.Sprintf("Failed to read response body: %v", err), dt())
					continue
				}
				if err := resp.Body.Close(); err != nil {
					addToHistograms(fmt.Sprintf("Failed to close response body: %v", err), dt())
					continue
				}
			}
			// status text
			var st string
			if err != nil {
				parts := strings.Split(err.Error(), ": ")
				st = parts[len(parts)-1]
			} else {
				st = fmt.Sprintf("%v %s", resp.StatusCode, http.StatusText(resp.StatusCode))
			}
			addToHistograms(st, dt())
		case "noop":
			addToHistograms("Did nothing", dt())
		default:
			addToHistograms(fmt.Sprintf("Unrecognized value for --fetcher: %q\n", *fetcher), dt())
		}
	}
}

func addToHistograms(s string, dt time.Duration) {
	addToStatusCodeHistogram(s)
	addToLatencyHistogram(dt)
}

// addToStatusCodeHistogram increments the given string in the histogram and
// then decrements it again after a second.
func addToStatusCodeHistogram(s string) {
	hmu.Lock()
	defer hmu.Unlock()
	histogram[s]++
	go func() {
		<-time.After(interval)
		hmu.Lock()
		defer hmu.Unlock()
		histogram[s]--
		if histogram[s] == 0 {
			delete(histogram, s)
		}
	}()
}

// addToLatencyHistogram works just like addToStatusCodeHistogram but for latencies.
func addToLatencyHistogram(dt time.Duration) {
	lmu.Lock()
	defer lmu.Unlock()
	latencies[dt]++
	go func() {
		<-time.After(interval)
		lmu.Lock()
		defer lmu.Unlock()
		latencies[dt]--
		if latencies[dt] == 0 {
			delete(latencies, dt)
		}
	}()
}

func sendTermboxInterrupts() {
	for _ = range time.Tick(500 * time.Millisecond) {
		termbox.Interrupt()
	}
}

// draw repaints the termbox UI, showing stats.
func draw(url string) {

	// Do the actual drawing.
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
	var p printer
	p.printf("%s", url)
	p.printf("%d workers", atomic.LoadInt64(numWorkers))
	p.printf("Results in past %v:", interval)

	lmu.Lock()
	defer lmu.Unlock()
	if len(latencies) == 0 {
		p.printf("  No responses")
	} else {
		maxDt := time.Duration(0)
		for dt := range latencies {
			if dt > maxDt {
				maxDt = dt
			}
		}
		p.printf("  Max latency: %v", maxDt)
	}

	hmu.Lock()
	defer hmu.Unlock()
	if len(histogram) == 0 {
		// Already reported above.
	} else {
		p.printf("  Responses:")
		var keys []string
		for k := range histogram {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p.printf("    %s: %d", k, histogram[k])
		}
	}
	p.printf("")
	termbox.Flush()
}

type printer struct {
	y int
}

func (p *printer) printf(fmat string, args ...interface{}) {
	tbprint(0, p.y, termbox.ColorDefault, termbox.ColorDefault, fmt.Sprintf(fmat, args...))
	p.y++
}

func tbprint(x, y int, fg, bg termbox.Attribute, msg string) {
	for _, c := range msg {
		termbox.SetCell(x, y, c, fg, bg)
		x++
	}
}
