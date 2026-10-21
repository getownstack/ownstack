package pluginlock

import (
	"strconv"
	"strings"
)

// Compare orders two Jenkins plugin or core versions the way Jenkins does
// (hudson.util.VersionNumber, a copy of Maven's ComparableVersion): it returns
// a negative number when a is older than b, 0 when they are equal, and a
// positive number when a is newer.
//
// It reads every format the update center uses: 1.27.26, 2.263.v941d21defef7,
// 4560.vc25b_83890c34, 2.30.1.83-289.v8426fcd19371, 1.0-beta-1, 1.0-SNAPSHOT.
// Numbers compare numerically at any length, trailing zeros do not count
// (1.0 == 1), qualifiers order snapshot < alpha < beta < milestone < rc <
// release < sp < anything else, and a '-' between digits opens a sub-version
// that sorts below a '.' item (1-1 < 1.1).
func Compare(a, b string) int {
	return compareItems(parseVersion(a), parseVersion(b))
}

type itemKind int

const (
	intItem itemKind = iota
	stringItem
	listItem
)

// item is one parsed component: a number (digits without leading zeros), a
// qualifier, or a sub-list opened by '-'.
type item struct {
	kind itemKind
	num  string
	str  string
	list []*item
}

var zeroItem = &item{kind: intItem, num: "0"}

// qualifiers in ascending order; "" is a release.
var qualifiers = []string{"snapshot", "alpha", "beta", "milestone", "rc", "", "sp"}

var qualifierAliases = map[string]string{"ga": "", "final": "", "cr": "rc"}

var releaseIndex = strconv.Itoa(indexOf(qualifiers, ""))

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func newInt(digits string) *item {
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		digits = "0"
	}
	return &item{kind: intItem, num: digits}
}

func newString(s string, followedByDigit bool) *item {
	if followedByDigit && len(s) == 1 {
		// a1 = alpha-1, b1 = beta-1, m1 = milestone-1
		switch s {
		case "a":
			s = "alpha"
		case "b":
			s = "beta"
		case "m":
			s = "milestone"
		}
	}
	if alias, ok := qualifierAliases[s]; ok {
		s = alias
	}
	return &item{kind: stringItem, str: s}
}

func parseItem(s string) *item {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return newString(s, false)
		}
	}
	return newInt(s)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// comparableQualifier maps a qualifier to a string that sorts in qualifier
// order; unknown qualifiers sort after every known one, alphabetically.
func comparableQualifier(q string) string {
	if i := indexOf(qualifiers, q); i >= 0 {
		return strconv.Itoa(i)
	}
	return strconv.Itoa(len(qualifiers)) + "-" + q
}

func (it *item) isNull() bool {
	switch it.kind {
	case intItem:
		return it.num == "0"
	case stringItem:
		return comparableQualifier(it.str) == releaseIndex
	default:
		return len(it.list) == 0
	}
}

// normalize drops trailing null items (0, release qualifiers, empty lists).
func (it *item) normalize() {
	for len(it.list) > 0 && it.list[len(it.list)-1].isNull() {
		it.list = it.list[:len(it.list)-1]
	}
}

func parseVersion(version string) *item {
	version = strings.ToLower(version)
	root := &item{kind: listItem}
	list := root
	stack := []*item{root}
	digit := false
	start := 0
	add := func(it *item) { list.list = append(list.list, it) }
	for i := 0; i < len(version); i++ {
		c := version[i]
		switch {
		case c == '.' || c == '-':
			if i == start {
				add(zeroItem)
			} else {
				add(parseItem(version[start:i]))
			}
			start = i + 1
			if c == '-' && digit {
				list.normalize() // 1.0-x = 1-x
				if i+1 < len(version) && isDigit(version[i+1]) {
					// A sub-list only between digits, to tell 1.1 from 1-1.
					sub := &item{kind: listItem}
					add(sub)
					list = sub
					stack = append(stack, sub)
				}
			}
		case isDigit(c):
			if !digit && i > start {
				add(newString(version[start:i], true))
				start = i
			}
			digit = true
		case isSpace(c):
			if i > start {
				if digit {
					add(parseItem(version[start:i]))
				} else {
					add(newString(version[start:i], true))
				}
				start = i
			}
			digit = false
		default:
			if digit && i > start {
				add(parseItem(version[start:i]))
				start = i
			}
			digit = false
		}
	}
	if len(version) > start {
		add(parseItem(version[start:]))
	}
	for i := len(stack) - 1; i >= 0; i-- {
		stack[i].normalize()
	}
	return root
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func compareNumbers(a, b string) int {
	if len(a) != len(b) {
		return sign(len(a) - len(b))
	}
	return strings.Compare(a, b)
}

// compareItems compares a with b, where b may be nil (a missing item).
func compareItems(a, b *item) int {
	switch a.kind {
	case intItem:
		if b == nil {
			if a.num == "0" {
				return 0
			}
			return 1
		}
		switch b.kind {
		case intItem:
			return compareNumbers(a.num, b.num)
		default: // 1.1 > 1-sp, 1.1 > 1-1
			return 1
		}
	case stringItem:
		if b == nil { // 1-rc < 1, 1-ga > 1
			return sign(strings.Compare(comparableQualifier(a.str), releaseIndex))
		}
		switch b.kind {
		case stringItem:
			return sign(strings.Compare(comparableQualifier(a.str), comparableQualifier(b.str)))
		default: // 1.any < 1.1, 1.any < 1-1
			return -1
		}
	default:
		if b == nil {
			if len(a.list) == 0 {
				return 0
			}
			return compareItems(a.list[0], nil)
		}
		switch b.kind {
		case intItem:
			return -1 // 1-1 < 1.1
		case stringItem:
			return 1 // 1-1 > 1-sp
		}
		for i := 0; i < len(a.list) || i < len(b.list); i++ {
			var l, r *item
			if i < len(a.list) {
				l = a.list[i]
			}
			if i < len(b.list) {
				r = b.list[i]
			}
			var c int
			switch {
			case l == nil && r == nil:
				c = 0
			case l == nil:
				c = -compareItems(r, nil)
			default:
				c = compareItems(l, r)
			}
			if c != 0 {
				return c
			}
		}
		return 0
	}
}
