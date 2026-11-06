package contract

import "testing"

func TestParseShard(t *testing.T) {
	good := []struct {
		in           string
		index, count int
	}{
		{"", 1, 1},
		{"1/1", 1, 1},
		{"1/4", 1, 4},
		{"4/4", 4, 4},
	}
	for _, c := range good {
		index, count, err := parseShard(c.in)
		if err != nil || index != c.index || count != c.count {
			t.Errorf("parseShard(%q) = %d, %d, %v; want %d, %d, nil", c.in, index, count, err, c.index, c.count)
		}
	}
	for _, in := range []string{"1", "0/4", "5/4", "1/0", "-1/4", "a/4", "1/b", "1/4/2", " 1/4", "/", "2/"} {
		if _, _, err := parseShard(in); err == nil {
			t.Errorf("parseShard(%q) is not an error", in)
		}
	}
}

// Every position lands in exactly one shard, and the shards differ in size
// by at most one.
func TestInShard(t *testing.T) {
	const total = 103
	for count := 1; count <= 5; count++ {
		sizes := make([]int, count)
		for pos := 0; pos < total; pos++ {
			hits := 0
			for index := 1; index <= count; index++ {
				if inShard(pos, index, count) {
					hits++
					sizes[index-1]++
				}
			}
			if hits != 1 {
				t.Fatalf("count %d: position %d is in %d shards", count, pos, hits)
			}
		}
		lo, hi := sizes[0], sizes[0]
		for _, s := range sizes {
			lo = min(lo, s)
			hi = max(hi, s)
		}
		if hi-lo > 1 {
			t.Errorf("count %d: shard sizes %v", count, sizes)
		}
	}
}
