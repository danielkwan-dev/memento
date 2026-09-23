// Command fixtures downloads real Letterboxd pages into the parser testdata
// directory. Run it manually when Letterboxd changes markup and a parser test
// starts failing; the tests themselves never touch the network.
//
//	go run ./cmd/fixtures -all
//	go run ./cmd/fixtures -film parasite-2019 -name film_parasite
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
)

const testdataDir = "internal/letterboxd/testdata"

// defaultFixtures is the full set the parser tests rely on. One entry per HTML
// *shape*, not per film: every film page uses the same template, so a second
// ordinary film would test nothing new.
var defaultFixtures = []struct {
	name string
	path string
	why  string
}{
	{"film_parasite", "/film/parasite-2019/", "happy path: all fields present"},
	{"film_stats_parasite", "/csi/film/parasite-2019/stats/", "stats endpoint (403s without TLS spoofing)"},
	{"film_multi_director", "/film/everything-everywhere-all-at-once/", "two directors"},
	{"film_silent", "/film/the-general/", "no spoken language"},
	{"films_grid_page1", "/dave/films/", "user grid, pagination present"},
	{"diary_page1", "/dave/films/diary/", "dated diary rows"},
	// TODO: these block persistently from some networks; re-capture
	// individually with -film when needed:
	//   film_no_runtime    /film/the-hire-ambush/
	//   film_documentary   /film/koyaanisqatsi/
}

func main() {
	var (
		all  = flag.Bool("all", false, "download the full default fixture set")
		film = flag.String("film", "", "film slug to download")
		user = flag.String("user", "", "username whose grid+diary to download")
		name = flag.String("name", "", "fixture basename (defaults to derived)")
		out  = flag.String("out", testdataDir, "output directory")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	client, err := letterboxd.NewClient(letterboxd.DefaultConfig(), log)
	if err != nil {
		fatal("build client: %v", err)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal("mkdir %s: %v", *out, err)
	}

	type job struct{ name, path string }
	var jobs []job

	switch {
	case *all:
		for _, f := range defaultFixtures {
			jobs = append(jobs, job{f.name, f.path})
		}
	case *film != "":
		n := *name
		if n == "" {
			n = "film_" + *film
		}
		jobs = append(jobs,
			job{n, "/film/" + *film + "/"},
			job{n + "_stats", "/csi/film/" + *film + "/stats/"},
		)
	case *user != "":
		n := *name
		if n == "" {
			n = *user
		}
		jobs = append(jobs,
			job{n + "_grid", "/" + *user + "/films/"},
			job{n + "_diary", "/" + *user + "/films/diary/"},
		)
	default:
		fmt.Fprintln(os.Stderr, "specify -all, -film <slug> or -user <name>")
		flag.Usage()
		os.Exit(2)
	}

	var failed int
	for _, j := range jobs {
		// Deliberately sequential with a pause: this is a manual maintenance
		// tool, so there is no reason to hammer the site.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		body, err := client.Get(ctx, letterboxd.BaseURL+j.path)
		cancel()
		if err != nil {
			log.Error("fetch failed", "fixture", j.name, "path", j.path, "err", err)
			failed++
			continue
		}
		dst := filepath.Join(*out, j.name+".html")
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			fatal("write %s: %v", dst, err)
		}
		log.Info("saved", "fixture", dst, "bytes", len(body))
		// Be generous: bursts of rapid requests trip Cloudflare rate limiting,
		// and this tool runs a handful of times a year.
		time.Sleep(5 * time.Second)
	}

	req, retries, blocks, switches := client.Snapshot()
	log.Info("done", "requests", req, "retries", retries,
		"blocks", blocks, "profile_switches", switches, "failed", failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
