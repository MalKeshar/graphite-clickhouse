package finder

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lomik/graphite-clickhouse/config"
	"github.com/lomik/graphite-clickhouse/helper/clickhouse"
	"github.com/lomik/graphite-clickhouse/pkg/scope"
	"github.com/lomik/graphite-clickhouse/pkg/where"
)

const ReverseLevelOffset = 10000
const TreeLevelOffset = 20000
const ReverseTreeLevelOffset = 30000

const DefaultTreeDate = "1970-02-12"

const (
	queryAuto     = config.IndexAuto
	queryDirect   = config.IndexDirect
	queryReversed = config.IndexReversed
)

type IndexFinder struct {
	url          string             // clickhouse dsn
	table        string             // graphite_tree table
	opts         clickhouse.Options // timeout, connectTimeout
	dailyEnabled bool
	confReverse  uint8
	confReverses config.IndexReverses
	reverse      uint8  // calculated in IndexFinder.useReverse only once
	body         []byte // clickhouse response body
	useDaily     bool
}

func NewIndex(url string, table string, dailyEnabled bool, reverse string, reverses config.IndexReverses, opts clickhouse.Options) Finder {
	return &IndexFinder{
		url:          url,
		table:        table,
		opts:         opts,
		dailyEnabled: dailyEnabled,
		confReverse:  config.IndexReverse[reverse],
		confReverses: reverses,
	}
}

func (idx *IndexFinder) where(query string, levelOffset int) *where.Where {
	level := strings.Count(query, ".") + 1

	w := where.New()

	w.And(where.Eq("Level", level+levelOffset))
	w.And(where.TreeGlob("Path", query))

	return w
}

func (idx *IndexFinder) checkReverses(query string) uint8 {
	for _, rule := range idx.confReverses {
		if len(rule.Prefix) > 0 && !strings.HasPrefix(query, rule.Prefix) {
			continue
		}
		if len(rule.Suffix) > 0 && !strings.HasSuffix(query, rule.Suffix) {
			continue
		}
		if rule.Regex != nil && rule.Regex.FindStringIndex(query) == nil {
			continue
		}
		return config.IndexReverse[rule.Reverse]
	}
	return idx.confReverse
}

func (idx *IndexFinder) useReverse(query string) bool {
	if idx.reverse == queryDirect {
		return false
	} else if idx.reverse == queryReversed {
		return true
	}

	if idx.reverse = idx.checkReverses(query); idx.reverse != queryAuto {
		return idx.useReverse(query)
	}

	w := where.IndexWildcard(query)
	if w == -1 {
		idx.reverse = queryDirect
		return idx.useReverse(query)
	}
	firstWildcardNode := strings.Count(query[:w], ".")

	w = where.IndexLastWildcard(query)
	lastWildcardNode := strings.Count(query[w:], ".")

	if firstWildcardNode < lastWildcardNode {
		idx.reverse = queryReversed
		return idx.useReverse(query)
	}
	idx.reverse = queryDirect
	return idx.useReverse(query)
}

func (idx *IndexFinder) Execute(ctx context.Context, query string, from int64, until int64) (err error) {
	idx.useReverse(query)

	if idx.dailyEnabled && from > 0 && until > 0 {
		idx.useDaily = true
	} else {
		idx.useDaily = false
	}

	var levelOffset int
	if idx.useDaily {
		if idx.useReverse(query) {
			levelOffset = ReverseLevelOffset
		}
	} else {
		if idx.useReverse(query) {
			levelOffset = ReverseTreeLevelOffset
		} else {
			levelOffset = TreeLevelOffset
		}
	}

	if idx.useReverse(query) {
		query = ReverseString(query)
	}

	w := idx.where(query, levelOffset)

	if idx.useDaily {
		w.Andf(
			"Date >='%s' AND Date <= '%s'",
			time.Unix(from, 0).Format("2006-01-02"),
			time.Unix(until, 0).Format("2006-01-02"),
		)
	} else {
		w.And(where.Eq("Date", DefaultTreeDate))
	}

	idx.body, err = clickhouse.Query(
		scope.WithTable(ctx, idx.table),
		idx.url,
		// TODO: consider consistent query generator
		fmt.Sprintf("SELECT Path FROM %s WHERE %s GROUP BY Path FORMAT TabSeparatedRaw", idx.table, w),
		idx.opts,
		nil,
	)

	return
}

func (idx *IndexFinder) Abs(v []byte) []byte {
	return v
}

func (idx *IndexFinder) makeList(onlySeries bool) [][]byte {
	if idx.body == nil {
		return [][]byte{}
	}

	rows := bytes.Split(idx.body, []byte{'\n'})

	skip := 0
	for i := 0; i < len(rows); i++ {
		if len(rows[i]) == 0 {
			skip++
			continue
		}
		if onlySeries && rows[i][len(rows[i])-1] == '.' {
			skip++
			continue
		}
		if skip > 0 {
			rows[i-skip] = rows[i]
		}
	}

	rows = rows[:len(rows)-skip]

	if idx.useReverse("") {
		for i := 0; i < len(rows); i++ {
			rows[i] = ReverseBytes(rows[i])
		}
	}

	return rows
}

func (idx *IndexFinder) List() [][]byte {
	return idx.makeList(false)
}

func (idx *IndexFinder) Series() [][]byte {
	return idx.makeList(true)
}
