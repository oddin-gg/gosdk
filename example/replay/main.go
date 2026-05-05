package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/protocols"
)

const defaultNodeID = 1

var (
	token  = "<your-access-token>"
	env    = protocols.IntegrationEnvironment
	region = protocols.DefaulRegion
	nodeID = defaultNodeID
)

func main() {
	initEnv()

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	cfg := gosdk.
		NewConfiguration(token, env, nodeID, false).
		SetRegion(region)

	switch cmd {
	case "list":
		runList(cfg)
	case "add":
		runAdd(cfg, args)
	case "remove":
		runRemove(cfg, args)
	case "clear":
		runClear(cfg)
	case "play":
		runPlay(cfg, args)
	case "stop":
		runStop(cfg)
	case "status":
		runStatus(cfg)
	case "listen":
		runListen(cfg, args)
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(
		os.Stderr, `Usage: %s <command> [args]

Commands:
  list                              GET /v1/replay (raw)
  add <urn>...                      PUT /v1/replay/events/{urn} for each
  remove <urn>...                   DELETE /v1/replay/events/{urn} for each
  clear                             POST /v1/replay/clear
  play [flags]                      POST /v1/replay/play
        -speed int               (default 1)
        -max-delay int           ms (default 30000)
        -rewrite-timestamps
        -product string          live|pre (default both)
        -parallel
  stop                              POST /v1/replay/stop
  status                            GET /v1/replay/status (raw XML)
  listen [<urn>... | all]           Subscribe to oddinreplay; filter by URNs
                                    (default: use current replay list)

Env: TOKEN, ENV={test|integration|production}, REGION={eu|ap}, NODE
`, os.Args[0],
	)
}

func initEnv() {
	if v := os.Getenv("TOKEN"); len(v) > 0 {
		token = v
	}
	if strings.HasPrefix(token, "<") {
		log.Fatal("TOKEN env var is required (or replace the placeholder in main.go)")
	}

	if v := strings.ToLower(os.Getenv("ENV")); len(v) > 0 {
		switch v {
		case "integration":
			env = protocols.IntegrationEnvironment
		case "production":
			env = protocols.ProductionEnvironment
		case "test":
			env = protocols.TestEnvironment
		default:
			log.Printf("ENV environment variable has invalid value %s, using default", v)
		}
	}

	if v := strings.ToLower(os.Getenv("REGION")); len(v) > 0 {
		switch v {
		case "ap":
			region = protocols.APSouthEast1
		case "eu":
			region = protocols.DefaulRegion
		default:
			log.Printf("REGION environment variable has invalid value %s, using default", v)
		}
	}

	if v := os.Getenv("NODE"); len(v) > 0 {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Printf("NODE environment variable has invalid value %s, using default", v)
		} else {
			nodeID = n
		}
	}
}

func newFeed(cfg protocols.OddsFeedConfiguration) protocols.OddsFeed {
	return gosdk.NewOddsFeed(cfg)
}

func parseURNs(args []string) ([]protocols.URN, error) {
	out := make([]protocols.URN, 0, len(args))
	for _, a := range args {
		u, err := protocols.ParseURN(a)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", a, err)
		}
		out = append(out, *u)
	}
	return out, nil
}

func runList(cfg protocols.OddsFeedConfiguration) {
	body, err := rawAPI(cfg, http.MethodGet, "/replay")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(body)
}

func runAdd(cfg protocols.OddsFeedConfiguration, args []string) {
	if len(args) == 0 {
		log.Fatal("add requires at least one URN")
	}
	urns, err := parseURNs(args)
	if err != nil {
		log.Fatal(err)
	}

	rm, err := newFeed(cfg).ReplayManager()
	if err != nil {
		log.Fatal(err)
	}
	for _, u := range urns {
		ok, err := rm.AddSportEventID(u)
		if err != nil {
			log.Fatalf("add %s: %v", u.ToString(), err)
		}
		fmt.Printf("added %s ok=%v\n", u.ToString(), ok)
	}
}

func runRemove(cfg protocols.OddsFeedConfiguration, args []string) {
	if len(args) == 0 {
		log.Fatal("remove requires at least one URN")
	}
	urns, err := parseURNs(args)
	if err != nil {
		log.Fatal(err)
	}

	rm, err := newFeed(cfg).ReplayManager()
	if err != nil {
		log.Fatal(err)
	}
	for _, u := range urns {
		ok, err := rm.RemoveSportEventID(u)
		if err != nil {
			log.Fatalf("remove %s: %v", u.ToString(), err)
		}
		fmt.Printf("removed %s ok=%v\n", u.ToString(), ok)
	}
}

func runClear(cfg protocols.OddsFeedConfiguration) {
	rm, err := newFeed(cfg).ReplayManager()
	if err != nil {
		log.Fatal(err)
	}
	ok, err := rm.Clear()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("clear ok=%v\n", ok)
}

func runPlay(cfg protocols.OddsFeedConfiguration, args []string) {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	speed := fs.Int("speed", 1, "replay speed multiplier")
	maxDelay := fs.Int("max-delay", 30000, "max inter-message delay (ms)")
	rewrite := fs.Bool("rewrite-timestamps", false, "rewrite message timestamps to now")
	product := fs.String("product", "", "live or pre; empty = both")
	parallel := fs.Bool("parallel", false, "run matches in parallel")
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}

	params := protocols.ReplayPlayParams{
		Speed:             speed,
		MaxDelayInMs:      maxDelay,
		RewriteTimestamps: rewrite,
		RunParallel:       parallel,
	}
	if *product != "" {
		params.Producer = product
	}

	rm, err := newFeed(cfg).ReplayManager()
	if err != nil {
		log.Fatal(err)
	}
	ok, err := rm.Play(params)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf(
		"play ok=%v speed=%d max_delay=%d rewrite=%v product=%q parallel=%v\n",
		ok, *speed, *maxDelay, *rewrite, *product, *parallel,
	)
}

func runStop(cfg protocols.OddsFeedConfiguration) {
	rm, err := newFeed(cfg).ReplayManager()
	if err != nil {
		log.Fatal(err)
	}
	ok, err := rm.Stop()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stop ok=%v\n", ok)
}

func runStatus(cfg protocols.OddsFeedConfiguration) {
	body, err := rawAPI(cfg, http.MethodGet, "/replay/status")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(body)
}

func runListen(cfg protocols.OddsFeedConfiguration, args []string) {
	filter, useFilter := buildFilter(cfg, args)
	if useFilter {
		if len(filter) == 0 {
			log.Println("filter is empty (no events in replay list); nothing will be printed")
		} else {
			log.Println("filtering messages to:")
			for u := range filter {
				log.Printf("  - %s", u.ToString())
			}
		}
	} else {
		log.Println("no filter — printing all messages on oddinreplay")
	}

	feed := newFeed(cfg)

	sb, err := feed.SessionBuilder()
	if err != nil {
		log.Fatal(err)
	}
	sessionCh, err := sb.BuildReplay()
	if err != nil {
		log.Fatal(err)
	}

	globalCh, err := feed.Open()
	if err != nil {
		log.Fatal(err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case sessionMsg, ok := <-sessionCh:
			if !ok {
				return
			}
			handleSessionMsg(sessionMsg, filter, useFilter)
		case <-globalCh:
			// Replay path uses dummy recovery; ignore global stream.
		case <-sigCh:
			log.Println("shutting down")
			signal.Stop(sigCh)
			_ = feed.Close()
			return
		}
	}
}

func handleSessionMsg(sessionMsg protocols.SessionMessage, filter map[protocols.URN]struct{}, useFilter bool) {
	if sessionMsg.UnparsableMessage != nil {
		raw := sessionMsg.UnparsableMessage.RawMessage()
		log.Printf("[unparsable] %s", leftN(string(raw), 256))
		return
	}

	switch msg := sessionMsg.Message.(type) {
	case protocols.OddsChange:
		printIfMatch("odds_change", eventID(msg.Event()), msg.RawMessage(), filter, useFilter)
	case protocols.FixtureChangeMessage:
		printIfMatch("fixture_change", eventID(msg.Event()), msg.RawMessage(), filter, useFilter)
	case protocols.BetCancel:
		printIfMatch("bet_cancel", eventID(msg.Event()), msg.RawMessage(), filter, useFilter)
	case protocols.BetSettlement:
		printIfMatch("bet_settlement", eventID(msg.Event()), msg.RawMessage(), filter, useFilter)
	case protocols.RollbackBetSettlement:
		printIfMatch("rollback_bet_settlement", eventID(msg.Event()), msg.RawMessage(), filter, useFilter)
	case protocols.RollbackBetCancel:
		printIfMatch("rollback_bet_cancel", eventID(msg.Event()), msg.RawMessage(), filter, useFilter)
	case protocols.BetStop:
		printIfMatch("bet_stop", eventID(msg.Event()), msg.RawMessage(), filter, useFilter)
	default:
		log.Printf("unknown msg type %T", msg)
	}
}

func eventID(ev any) protocols.URN {
	se, ok := ev.(protocols.SportEvent)
	if !ok {
		return protocols.URN{}
	}
	return se.ID()
}

func printIfMatch(kind string, eventID protocols.URN, raw []byte, filter map[protocols.URN]struct{}, useFilter bool) {
	if useFilter {
		if _, ok := filter[eventID]; !ok {
			return
		}
	}
	log.Printf("[%s] %s | %s", kind, eventID.ToString(), leftN(string(raw), 512))
}

func buildFilter(cfg protocols.OddsFeedConfiguration, args []string) (map[protocols.URN]struct{}, bool) {
	if len(args) == 1 && args[0] == "all" {
		return nil, false
	}
	if len(args) > 0 {
		urns, err := parseURNs(args)
		if err != nil {
			log.Fatal(err)
		}
		set := make(map[protocols.URN]struct{}, len(urns))
		for _, u := range urns {
			set[u] = struct{}{}
		}
		return set, true
	}

	rm, err := newFeed(cfg).ReplayManager()
	if err != nil {
		log.Fatal(err)
	}
	events, err := rm.ReplayList()
	if err != nil {
		log.Fatalf("fetch replay list for filter: %v", err)
	}
	set := make(map[protocols.URN]struct{}, len(events))
	for _, e := range events {
		set[e.ID()] = struct{}{}
	}
	return set, true
}

func rawAPI(cfg protocols.OddsFeedConfiguration, method, path string) (string, error) {
	host, err := cfg.APIURL()
	if err != nil {
		return "", err
	}

	url := "https://" + host + "/v1" + path
	if cfg.SdkNodeID() != nil {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url = fmt.Sprintf("%s%snode_id=%d", url, sep, *cfg.SdkNodeID())
	}

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("accept", "application/xml")
	if tok := cfg.AccessToken(); tok != nil {
		req.Header.Set("x-access-token", *tok)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s %s -> %d: %s", method, url, resp.StatusCode, string(body))
	}
	return string(body), nil
}

func leftN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
