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
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"go.yaml.in/yaml/v3"
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
	if flag.NArg() == 0 {
		fmt.Println("every rule has a firing test case; no scrape was given, so the metrics were not checked")
		return
	}
	fmt.Printf("every rule has a firing test case, and names only what the %d scrapes export\n", flag.NArg())
}

// read returns a scrape from a file, or from an http(s) URL.
func read(src string) ([]byte, error) {
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		return os.ReadFile(src)
	}
	resp, err := http.Get(src) //nolint:gosec // the URL is the operator's argument
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", src, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

type ruleFile struct {
	Groups []struct {
		Rules []struct {
			Alert string `yaml:"alert"`
			Expr  string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

type testFile struct {
	Tests []struct {
		AlertRuleTest []struct {
			Alertname string `yaml:"alertname"`
			ExpAlerts []any  `yaml:"exp_alerts"`
		} `yaml:"alert_rule_test"`
	} `yaml:"tests"`
}

// untested returns, sorted, the alerts of rules that no test case of tests expects to fire.
func untested(rules, tests []byte) ([]string, error) {
	var r ruleFile
	if err := yaml.Unmarshal(rules, &r); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	var tf testFile
	if err := yaml.Unmarshal(tests, &tf); err != nil {
		return nil, fmt.Errorf("tests: %w", err)
	}
	fires := map[string]bool{}
	for _, t := range tf.Tests {
		for _, c := range t.AlertRuleTest {
			if len(c.ExpAlerts) > 0 {
				fires[c.Alertname] = true
			}
		}
	}
	var out []string
	for _, g := range r.Groups {
		for _, rule := range g.Rules {
			if rule.Alert != "" && !fires[rule.Alert] {
				out = append(out, rule.Alert)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// exported is what the scrapes export: metric name, then label, then its values.
type exported map[string]map[string]map[string]bool

func (e exported) add(metric, label, value string) {
	if e[metric] == nil {
		e[metric] = map[string]map[string]bool{}
	}
	if e[metric][label] == nil {
		e[metric][label] = map[string]bool{}
	}
	if value != "" {
		e[metric][label][value] = true
	}
}

// exports reads scrapes in the Prometheus text format. A histogram's family is also its
// _bucket (with le), _sum and _count series.
func exports(scrapes ...[]byte) (exported, error) {
	e := exported{}
	for _, b := range scrapes {
		parser := expfmt.NewTextParser(model.UTF8Validation)
		fams, err := parser.TextToMetricFamilies(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("a scrape: %w", err)
		}
		for name, f := range fams {
			names := []string{name}
			if f.GetType() == dto.MetricType_HISTOGRAM {
				names = append(names, name+"_bucket", name+"_sum", name+"_count")
				e.add(name+"_bucket", "le", "")
			}
			for _, n := range names {
				if e[n] == nil {
					e[n] = map[string]map[string]bool{}
				}
				for _, m := range f.GetMetric() {
					for _, l := range m.GetLabel() {
						e.add(n, l.GetName(), l.GetValue())
					}
				}
			}
		}
	}
	return e, nil
}

// unexported returns, for every rule, each metric, label value and grouping label it names
// that none of the scrapes exports.
func unexported(rules []byte, scrapes ...[]byte) ([]string, error) {
	var r ruleFile
	if err := yaml.Unmarshal(rules, &r); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	e, err := exports(scrapes...)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, g := range r.Groups {
		for _, rule := range g.Rules {
			sel, grouping, err := selectors(rule.Expr)
			if err != nil {
				out = append(out, fmt.Sprintf("%s: %v", rule.Alert, err))
				continue
			}
			for _, s := range sel {
				out = append(out, e.check(rule.Alert, s, grouping)...)
			}
		}
	}
	return out, nil
}

func (e exported) check(alert string, s selector, grouping []string) []string {
	labels, ok := e[s.metric]
	if !ok {
		return []string{fmt.Sprintf("%s: no binary exports the metric %s", alert, s.metric)}
	}
	var out []string
	for _, m := range s.matchers {
		values, ok := labels[m.label]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s: %s has no label %s", alert, s.metric, m.label))
		case m.op == "=" && !values[m.value]:
			out = append(out, fmt.Sprintf("%s: %s{%s} never has the value %q", alert, s.metric, m.label, m.value))
		case m.op == "=~":
			for _, v := range strings.Split(m.value, "|") {
				if !plainValue.MatchString(v) {
					out = append(out, fmt.Sprintf("%s: %s{%s=~%q}: the check reads only alternatives of plain values", alert, s.metric, m.label, m.value))
					break
				}
				if !values[v] {
					out = append(out, fmt.Sprintf("%s: %s{%s} never has the value %q", alert, s.metric, m.label, v))
				}
			}
		}
	}
	for _, l := range grouping {
		if _, ok := labels[l]; !ok {
			out = append(out, fmt.Sprintf("%s: groups by %s, a label %s does not have", alert, l, s.metric))
		}
	}
	return out
}

var plainValue = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// selector is a metric a rule names, with its label matchers.
type selector struct {
	metric   string
	matchers []matcher
}

type matcher struct{ label, op, value string }

// keywords of PromQL that are not metric names: the aggregations, which may be followed by
// "by (...)" before their "(", and the other keywords.
var keywords = map[string]bool{"sum": true, "min": true, "max": true, "avg": true, "count": true, "stddev": true,
	"stdvar": true, "group": true, "topk": true, "bottomk": true, "quantile": true, "count_values": true,
	"limitk": true, "limit_ratio": true, "by": true, "without": true, "on": true, "ignoring": true,
	"group_left": true, "group_right": true, "bool": true, "and": true, "or": true, "unless": true, "offset": true}

var token = regexp.MustCompile(`\s+|[A-Za-z_:][A-Za-z0-9_:]*|"(?:[^"\\]|\\.)*"|=~|!~|!=|==|>=|<=|[0-9.]+[a-z]*|.`)

// selectors reads the metrics, their matchers and the grouping labels of a PromQL expression.
// It reads the PromQL that the rules use: selectors with matchers, functions, aggregations
// with by or without, ranges, numbers and operators. A grouping label is checked against
// every metric of the expression.
func selectors(expr string) ([]selector, []string, error) {
	var toks []string
	for _, t := range token.FindAllString(expr, -1) {
		if strings.TrimSpace(t) != "" {
			toks = append(toks, t)
		}
	}
	var sel []selector
	var grouping []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		next := ""
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		switch {
		case t == "[": // a range or a subquery: skip to its end
			for i < len(toks) && toks[i] != "]" {
				i++
			}
		case (t == "by" || t == "without" || t == "on" || t == "ignoring") && next == "(":
			for i += 2; i < len(toks) && toks[i] != ")"; i++ {
				if toks[i] != "," {
					grouping = append(grouping, toks[i])
				}
			}
		case !identifier.MatchString(t) || keywords[t] || next == "(":
			// an operator, a number, a keyword, or a function or aggregation
		default:
			s := selector{metric: t}
			if next == "{" {
				for i += 2; i < len(toks) && toks[i] != "}"; i++ {
					if toks[i] == "," {
						continue
					}
					if i+2 >= len(toks) || !strings.HasPrefix(toks[i+2], `"`) {
						return nil, nil, fmt.Errorf("a matcher of %s the check cannot read", t)
					}
					s.matchers = append(s.matchers, matcher{label: toks[i], op: toks[i+1], value: strings.Trim(toks[i+2], `"`)})
					i += 2
				}
			}
			sel = append(sel, s)
		}
	}
	return sel, grouping, nil
}

var identifier = regexp.MustCompile(`^[A-Za-z_:][A-Za-z0-9_:]*$`)
