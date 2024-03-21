package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	colly "github.com/gocolly/colly/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	maxVisitsCst = 10000

	site = "https://www.pbs.org/newshour/ "
	//site           = "https://www.pbs.org/newshour/video"
	// Full list
	//site = "https://www.pbs.org/shows/?search=&genre=all-genres&source=all-sources&sortBy=popular&stationId=057f6e22-7d9c-4fd6-b968-bc17102698de"
	// PBS kids
	//site = "https://pbskids.org/video/"
	//site = "https://pbskids.org/video/lets-go-luna/"
	//site = "https://pbskids.org/video/super-why/"

	// BBC video search
	// https://www.bbc.co.uk/search?q=video&d=NEWS_GNL
	// https://www.bbc.com/news/av/10462520

	allowedDomains = "pbs.org"
	parallelism    = 5
	maxDepth       = 3
	cacheDir       = "./cache/"

	pbsRegexFilter = `^https://[^\/]*\.pbs.org.*`
	//pbsRegexFilter = `^https://.*\.pbs.org.*`
	//pbsRegexFilter = `https://.*\\.pbs.org/.*`
	// Ref: https://github.com/gocolly/colly/issues/544

	specificFollow = "https://www.pbs.org/newshour/show/"
	//specificFollow = "https://www.pbs.org/newshour/show/the-impact-of-israeli-governments-controversial-plan"

	stripHashRegexFilter = `#.*`

	hrefContains = "newshour"
	pbsIFrameSrc = "player.pbs.org"

	httpProxyURL = "http://localhost:8088"

	redirectRegexFilter = `(https://urs\.pbs\.org/redirect/[^"]+)`

	outFileCst            = "./output/outfile.json"
	promMetricsOutFileCst = "./output/promMetrics"
	filePermissionsCst    = 0644

	proxyEnable = false

	debugLevelCst = 11

	signalChannelSize = 10

	promListenCst           = ":9991"
	promPathCst             = "/metrics"
	promMaxRequestsInFlight = 10
	promEnableOpenMetrics   = true

	quantileError    = 0.05
	summaryVecMaxAge = 5 * time.Minute
)

type Redirects struct {
	Url  string `json:"url"`
	Info string `json:"info"`
}

// https://github.com/gocolly/colly/blob/master/_examples/parallel/parallel.go

var (
	// Passed by "go build -ldflags" for the show version
	commit string
	date   string

	debugLevel int

	pbsRegex       *regexp.Regexp
	stripHashRegex *regexp.Regexp
	redirectRegex  *regexp.Regexp

	onRequestCount uint64
	visitCount     uint64

	hrefCount        uint64
	hrefMatchCount   uint64
	hrefNoMatchCount uint64

	followedCount uint64
	hrefIncCount  uint64

	iframeCount    uint64
	iframePBSCount uint64

	scriptCount            uint64
	scriptVideoBridgeCount uint64

	once sync.Once

	vistedMap sync.Map

	maxVisits uint64

	output []Redirects

	pC = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: "counters",
			Name:      "pbsColly",
			Help:      "pbsColly counters",
		},
		[]string{"function", "variable", "type"},
	)
	pH = promauto.NewSummaryVec(
		prometheus.SummaryOpts{
			Subsystem: "histrograms",
			Name:      "pbsColly",
			Help:      "pbsColly historgrams",
			Objectives: map[float64]float64{
				0.1:  quantileError,
				0.5:  quantileError,
				0.99: quantileError,
			},
			MaxAge: summaryVecMaxAge,
		},
		[]string{"function", "variable", "type"},
	)
)

func debugLog(logIt bool, str string) {
	if logIt {
		log.Print(str)
	}
}

func main() {

	log.Printf("colly crawling site: %s\n", site)

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	go initSignalHandler(cancel)

	version := flag.Bool("version", false, "version")

	mv := flag.Uint64("mv", maxVisitsCst, "max visits")

	// https://pkg.go.dev/net#Listen
	promListen := flag.String("promListen", promListenCst, "Prometheus http listening socket")
	promPath := flag.String("promPath", promPathCst, "Prometheus http path. Default = /metrics")
	// curl -s http://[::1]:9111/metrics 2>&1 | grep -v "#"
	// curl -s http://127.0.0.1:9111/metrics 2>&1 | grep -v "#"

	outFile := flag.String("outFile", outFileCst, "Outfile json filename")
	promOutFile := flag.String("promOutFile", promMetricsOutFileCst, "output for prom metrics")

	dl := flag.Int("dl", debugLevelCst, "nasty debugLevel")

	maxVisits = *mv
	debugLevel = *dl

	flag.Parse()

	if *version {
		log.Println("commit:", commit, "\tdate(UTC):", date)
		os.Exit(0)
	}

	go initPromHandler(*promPath, *promListen)

	defer printSummary()

	compileRegex()

	// https://pkg.go.dev/github.com/gocolly/colly#Collector
	// http://go-colly.org/docs/best_practices/extensions/
	c := colly.NewCollector(
		//colly.AllowURLRevisit(),
		colly.URLFilters(
			regexp.MustCompile(pbsRegexFilter),
		),
		//colly.AllowedDomains(allowedDomains),
		colly.Async(),
		colly.MaxDepth(maxDepth),
		colly.IgnoreRobotsTxt(),
		colly.CacheDir(cacheDir),
	)

	c.WithTransport(&http.Transport{
		//Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	})

	err := c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: parallelism,
		//Delay:      5 * time.Second,
		//RandomDelay: 5 * time.Second,
	})
	if err != nil {
		log.Fatal("c.Limit(&colly.LimitRule err:", err)
	}

	if proxyEnable {
		err := c.SetProxy(httpProxyURL)
		if err != nil {
			log.Fatal("c.SetProxy(httpProxyURL) err:", err)
		}
	}

	//c.OnRequest(func(r *colly.Request) {
	c.OnRequest(onRequest)

	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		handleHref(e)
	})

	c.OnHTML("script", func(e *colly.HTMLElement) {
		handleScript(e)
	})

	c.OnHTML("iframe", func(e *colly.HTMLElement) {
		handleIframe(e)
	})

	debugLog(debugLevel > 10, fmt.Sprintf("main() c.Visit(%s)", site))
	errV := c.Visit(site)
	if errV != nil {
		log.Printf("c.Visit(%s) errV:%v", site, errV)
	}

	c.Wait()

	debugLog(debugLevel > 10, "main() json.MarshalIndent")
	json_data, err := json.MarshalIndent(output, "", " ")
	if err != nil {
		log.Println(err)
	}

	debugLog(debugLevel > 10, fmt.Sprintf("main() json os.WriteFile:%s", *outFile))
	err = os.WriteFile(*outFile, json_data, filePermissionsCst)
	if err != nil {
		log.Println(err)
	}

	promMetricsData := selfRequestPromMetrics(*promPath, *promListen)

	debugLog(debugLevel > 10, fmt.Sprintf("main() prom metrics os.WriteFile:%s", *promOutFile))
	if err := os.WriteFile(*promOutFile, promMetricsData, filePermissionsCst); err != nil {
		log.Fatal(err)
	}
}

func selfRequestPromMetrics(promPath string, promListen string) (resBody []byte) {

	requestURL := fmt.Sprintf("http://localhost%s%s", promListen, promPath)

	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		log.Fatalf("selfRequestPromMetrics NewRequest: could not create request: %s\n", err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("selfRequestPromMetrics Do: error making http request: %s\n", err)

	}

	debugLog(debugLevel > 10, "selfRequestPromMetrics: got response!\n")
	debugLog(debugLevel > 10, fmt.Sprintf("selfRequestPromMetrics: status code: %d\n", res.StatusCode))

	resBody, errR := io.ReadAll(res.Body)
	if errR != nil {
		log.Fatalf("selfRequestPromMetrics: could not read response body: %s\n", errR)
	}
	//fmt.Printf("client: response body: %s\n", resBody)
	res.Body.Close()

	return resBody
}

func onRequest(r *colly.Request) {

	startTime := time.Now()
	defer func() {
		pH.WithLabelValues("onRequest", "start", "complete").Observe(time.Since(startTime).Seconds())
	}()
	pC.WithLabelValues("onRequest", "start", "counter").Inc()

	atomic.AddUint64(&onRequestCount, 1)
	if debugLevel > 10 {
		log.Printf("onRequest,r:%s\n", r.URL)
		//log.Printf("onRequest,r:%v\n", r)
	}

	debugLog(debugLevel > 10, fmt.Sprintf("onRequest() colly.Request:%s", r.URL))

	debugLog(debugLevel > 10, fmt.Sprintf("onRequest() onRequestCount:%d > maxVisits:%d", onRequestCount, maxVisits))
	if onRequestCount > maxVisits {

		// once.Do(func() {
		// 	log.Println("Max visits reached:", maxVisits)

		// 	c.Limit(&colly.LimitRule{
		// 		DomainGlob:  "*",
		// 		Parallelism: parallelism,
		// 		//Delay:      5 * time.Second,
		// 		RandomDelay: 1 * time.Second,
		// 	})
		// })

		once.Do(func() {
			log.Println("Max visits reached:", maxVisits)

			printSummary()

			os.Exit(0)
		})
	}

	r.Headers.Set("Accept-Language", "en-US")
}

func handleHref(e *colly.HTMLElement) {

	startTime := time.Now()
	defer func() {
		pH.WithLabelValues("handleHref", "start", "complete").Observe(time.Since(startTime).Seconds())
	}()
	pC.WithLabelValues("handleHref", "start", "counter").Inc()

	atomic.AddUint64(&hrefCount, 1)
	link := e.Attr("href")

	//if debugLevel > 10 {
	if debugLevel > 10 && followedCount == 0 {
		log.Printf("handleHref, e.Index:%d, hrefCount:%d, link:%s\n", e.Index, hrefCount, link)
		//log.Printf("---------------------\nhandleHref, hrefCount:%d, e:%v\n",hrefCount, e)
	}

	if pbsRegex.MatchString(link) {

		atomic.AddUint64(&hrefMatchCount, 1)

		stripppedLink := stripHashRegex.ReplaceAllString(link, "")
		if debugLevel > 100 {
			log.Printf("link:%s\n", link)
			log.Printf("stripppedLink:%s\n", stripppedLink)
		}

		i, loaded := vistedMap.LoadOrStore(stripppedLink, int64(1))
		if !loaded {

			if strings.Contains(link, specificFollow) {

				if debugLevel > 10 {
					//log.Printf("FOUND, i:%d\n",i)
					log.Printf("Visit:%s\n", link)
				}

				err := e.Request.Visit(link)
				if err != nil {
					log.Printf("handleHref c.Request(%s) err:%v", err, site)
				}
				atomic.AddUint64(&visitCount, 1)

			} else {
				if debugLevel > 100 {
					log.Printf("NOT found link:%s\n", link)
				}
			}
			return
		}

		if debugLevel > 100 {
			log.Printf("loaded, link:%s, i:%d\n", link, i)
		}

		vistedMap.CompareAndSwap(link, i, i.(int64)+1)
		atomic.AddUint64(&hrefIncCount, 1)

		return
	}

	atomic.AddUint64(&hrefNoMatchCount, 1)
	if debugLevel > 100 {
		if len(link) > 50 {
			log.Printf("NOT pbsRegex.MatchString(link):%s\n", link[0:50])
		} else {
			log.Printf("NOT pbsRegex.MatchString(link):%s\n", link)
		}
	}
}

func handleScript(e *colly.HTMLElement) {

	startTime := time.Now()
	defer func() {
		pH.WithLabelValues("handleScript", "start", "complete").Observe(time.Since(startTime).Seconds())
	}()
	pC.WithLabelValues("handleScript", "start", "counter").Inc()

	atomic.AddUint64(&scriptCount, 1)
	if debugLevel > 100 {
		log.Printf("handleScript,e:%v\n", e)
	}

	if iframePBSCount > 0 {

		text := e.Text

		if strings.Contains(text, "window.videoBridge") {
			atomic.AddUint64(&scriptVideoBridgeCount, 1)

			if debugLevel > 100 {
				log.Println("*************************************")
				log.Printf("script e:%v\n", e)
				log.Println("*************************************")
				//log.Printf("script e.Text:%v\n",e.Text)
			}

			log.Println("-------------------------------------------------------")
			log.Printf("script window.videoBridge text:%v\n", text)
			log.Println("-------------------------------------------------------")

			matches := redirectRegex.FindAllString(text, -1)
			if debugLevel > 100 {
				if matches != nil {
					log.Printf("script window.videoBridge matches:%v\n", matches)
				} else {
					log.Printf("script window.videoBridge no matches\n")
				}
			}
			for _, match := range matches {

				output = append(output, Redirects{Url: match})
			}

			time.Sleep(5 * time.Second)
		}
	}
}

func handleIframe(e *colly.HTMLElement) {

	startTime := time.Now()
	defer func() {
		pH.WithLabelValues("handleIframe", "start", "complete").Observe(time.Since(startTime).Seconds())
	}()
	pC.WithLabelValues("handleIframe", "start", "counter").Inc()

	atomic.AddUint64(&iframeCount, 1)

	//log.Printf("e:%v\n",e)

	iframeSrc := e.Attr("src")

	if debugLevel > 100 {
		log.Printf("handleIframe, src:%s\n", iframeSrc)
		//log.Printf("handleIframe,e:%v\n", e)
	}

	if strings.Contains(iframeSrc, pbsIFrameSrc) {
		if debugLevel > 10 {
			log.Println("=============================")
			log.Println("player.pbs.org:", iframeSrc)
		}

		debugLog(debugLevel > 10, fmt.Sprintf("handleIframe() c.Visit(%s)", iframeSrc))
		errV := e.Request.Visit(iframeSrc)
		if errV != nil {
			log.Printf("e.Request.Visit(iframeSrc:%s)) errV:%v", iframeSrc, errV)
		}

		//d.Visit(iframeSrc)
		atomic.AddUint64(&iframePBSCount, 1)
	}
}

func compileRegex() {

	pC.WithLabelValues("handleIframe", "start", "counter").Inc()

	var cErr error

	if debugLevel > 10 {
		log.Printf("compileRegex\n")
	}

	pbsRegex, cErr = regexp.Compile(pbsRegexFilter)
	if cErr != nil {
		panic(cErr)
	}

	stripHashRegex, cErr = regexp.Compile(stripHashRegexFilter)
	if cErr != nil {
		panic(cErr)
	}

	redirectRegex, cErr = regexp.Compile(redirectRegexFilter)
	if cErr != nil {
		panic(cErr)
	}
}

func printSummary() {

	pC.WithLabelValues("handleIframe", "start", "counter").Inc()

	log.Println("-------------------------------------------------------")
	log.Printf("onRequestCount:%d\n\n", onRequestCount)

	log.Printf("hrefCount:%d\n", hrefCount)
	log.Printf("hrefMatchCount:%d\n", hrefMatchCount)
	log.Printf("hrefNoMatchCount:%d\n\n", hrefNoMatchCount)

	log.Printf("visitCount:%d\n", visitCount)
	log.Printf("hrefIncCount:%d\n\n", hrefIncCount)

	log.Printf("iframeCount:%d\n", iframeCount)
	log.Printf("iframePBSCount:%d\n\n", iframePBSCount)

	log.Printf("scriptCount:%d\n", scriptCount)
	log.Printf("scriptVideoBridgeCount:%d\n", scriptVideoBridgeCount)
	log.Println("-------------------------------------------------------")
}

// //https://github.com/jhnwr/scrapingexamples/blob/main/federbridge/main.go
// d := c.Clone()

// d.OnRequest(func(r *colly.Request) {
// 	atomic.AddInt64(&dVisitCount, 1)

// 	if dVisitCount > maxVisits {
// 		log.Println("Max visits reached:", maxVisits)
// 		os.Exit(0)
// 	}

// 	if dPrintFoundLink {
// 		log.Println("Visiting:", r.URL)
// 	}

// 	r.Headers.Set("Accept-Language", "en-US")
// })

// c.OnXML("/html/body", func(e *colly.XMLElement) {
//     child := e.ChildText("div[3]/main/div/div[1]/div[1]/div[1]/div")
//     log.Println("child:",child)
// })

// initSignalHandler sets up signal handling for the process, and
// will call cancel() when recieved
func initSignalHandler(cancel context.CancelFunc) {

	c := make(chan os.Signal, signalChannelSize)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	<-c
	log.Printf("Signal caught, closing application")
	cancel()

	os.Exit(0)
}

// initPromHandler starts the prom handler with error checking
func initPromHandler(promPath string, promListen string) {
	// https: //pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp?tab=doc#HandlerOpts
	http.Handle(promPath, promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			EnableOpenMetrics:   promEnableOpenMetrics,
			MaxRequestsInFlight: promMaxRequestsInFlight,
		},
	))
	go func() {
		err := http.ListenAndServe(promListen, nil)
		if err != nil {
			log.Fatal("prometheus error", err)
		}
	}()
}
