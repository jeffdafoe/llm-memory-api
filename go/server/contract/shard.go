package contract

import (
	"fmt"
	"strconv"
	"strings"
)

// parseShard reads CONTRACT_SHARD, "i/n" with 1 <= i <= n: run the i-th of
// n shards. Empty means one shard holding every scenario. Anything else is
// an error, so a mistyped CI matrix fails instead of running all scenarios
// on every shard or none on some.
func parseShard(v string) (index, count int, err error) {
	if v == "" {
		return 1, 1, nil
	}
	is, ns, ok := strings.Cut(v, "/")
	if ok {
		index, err = strconv.Atoi(is)
	}
	if ok && err == nil {
		count, err = strconv.Atoi(ns)
	}
	if !ok || err != nil || count < 1 || index < 1 || index > count {
		return 0, 0, fmt.Errorf("CONTRACT_SHARD %q: want i/n with 1 <= i <= n", v)
	}
	return index, count, nil
}

// inShard reports whether the scenario at position pos in Scenarios belongs
// to shard index of count. Round-robin, so the slow cases that sit together
// in one cases_*.go file spread over all the shards.
func inShard(pos, index, count int) bool {
	return pos%count == index-1
}
