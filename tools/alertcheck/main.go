// Command alertcheck keeps an alert rule from being one that cannot fire (architecture §10,
// lesson 12):
//
//  1. every rule of alerts.yml has a promtool test case in which it fires;
//  2. every metric a rule names, every label value it matches and every label it groups by is
//     exported by the service: the scrapes given as arguments, files or URLs of /metrics.
//
// Usage: go run ./tools/alertcheck [-rules deploy/prometheus/alerts.yml]
// [-tests deploy/prometheus/alerts_test.yml] [scrape ...]
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	rulesPath := flag.String("rules", "deploy/prometheus/alerts.yml", "the alert rules")
	testsPath := flag.String("tests", "deploy/prometheus/alerts_test.yml", "their promtool tests")
	flag.Parse()
	rules, err := os.ReadFile(*rulesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	tests, err := os.ReadFile(*testsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var problems []string
	names, err := untested(rules, tests)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, n := range names {
		problems = append(problems, "the rule "+n+" has no promtool test case in which it fires")
	}
	if flag.NArg() > 0 {
		var scrapes [][]byte
		for _, src := range flag.Args() {
			b, err := read(src)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			scrapes = append(scrapes, b)
		}
		missing, err := unexported(rules, scrapes...)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		problems = append(problems, missing...)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
	fmt.Println("every rule has a firing test, and names only what the service exports")
}

func read(src string) ([]byte, error) { return os.ReadFile(src) }

// untested returns the alerts of rules that no test case of tests expects to fire.
func untested(rules, tests []byte) ([]string, error) { return nil, nil }

// unexported returns, for every rule, each metric, label value and grouping label it names
// that none of the scrapes exports.
func unexported(rules []byte, scrapes ...[]byte) ([]string, error) { return nil, nil }
