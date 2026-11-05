package jsregex

// Safe is safe-regex 2.1.1's verdict on a valid pattern: unsafe when
// quantifiers nest (star height above 1) or there are more than 25 of them,
// counted on the parse tree. A pattern its parser (regexp-tree) cannot read
// is reported safe, as safe-regex does.
func (p *Pattern) Safe() bool {
	if p.regexpTreeFails {
		return true
	}
	count, height := 0, 0
	var walk func(n *node, depth int)
	walk = func(n *node, depth int) {
		if n == nil {
			return
		}
		if n.kind == kRepeat {
			count++
			depth++
			if depth > height {
				height = depth
			}
		}
		walk(n.child, depth)
		for _, alt := range n.alts {
			for _, t := range alt {
				walk(t, depth)
			}
		}
	}
	walk(p.root, 0)
	return height <= 1 && count <= 25
}
