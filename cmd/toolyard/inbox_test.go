package main

import (
	"flag"
	"reflect"
	"testing"
	"time"
)

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "")
	mode := fs.String("mode", "any", "")
	pos := parseInterspersed(fs, []string{"rq_1", "--timeout", "2s", "rq_2", "--mode", "all"})
	if !reflect.DeepEqual(pos, []string{"rq_1", "rq_2"}) || *timeout != 2*time.Second || *mode != "all" {
		t.Fatalf("got pos=%v timeout=%v mode=%v", pos, *timeout, *mode)
	}
}
