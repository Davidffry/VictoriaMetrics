package metricsql

import (
	"fmt"
	"sort"
	"strings"
)

// Optimize optimizes e in order to improve its performance.
//
// It performs the following optimizations:
//
//   - Adds missing filters to `foo{filters1} op bar{filters2}`
//     according to https://utcc.utoronto.ca/~cks/space/blog/sysadmin/PrometheusLabelNonOptimization
//     I.e. such query is converted to `foo{filters1, filters2} op bar{filters1, filters2}`
func Optimize(e Expr) Expr {
	if !canOptimize(e) {
		return e
	}
	eCopy := Clone(e)
	optimizeInplace(eCopy)
	return eCopy
}

func canOptimize(e Expr) bool {
	switch t := e.(type) {
	case *RollupExpr:
		return canOptimize(t.Expr) || canOptimize(t.At)
	case *FuncExpr:
		for _, arg := range t.Args {
			if canOptimize(arg) {
				return true
			}
		}
	case *AggrFuncExpr:
		for _, arg := range t.Args {
			if canOptimize(arg) {
				return true
			}
		}
	case *BinaryOpExpr:
		return true
	}
	return false
}

// Clone clones the given expression e and returns the cloned copy.
func Clone(e Expr) Expr {
	s := e.AppendString(nil)
	eCopy, err := Parse(string(s))
	if err != nil {
		panic(fmt.Errorf("BUG: cannot parse the expression %q: %w", s, err))
	}
	return eCopy
}

func optimizeInplace(e Expr) {
	switch t := e.(type) {
	case *RollupExpr:
		optimizeInplace(t.Expr)
		optimizeInplace(t.At)
	case *FuncExpr:
		for _, arg := range t.Args {
			optimizeInplace(arg)
		}
	case *AggrFuncExpr:
		for _, arg := range t.Args {
			optimizeInplace(arg)
		}
	case *BinaryOpExpr:
		optimizeInplace(t.Left)
		optimizeInplace(t.Right)
		lfs := getCommonLabelFilters(t)
		pushdownBinaryOpFiltersInplace(t, lfs)
	}
}

func getCommonLabelFilters(e Expr) []LabelFilter {
	switch t := e.(type) {
	case *MetricExpr:
		return getCommonLabelFiltersWithoutMetricName(t.LabelFilterss)
	case *RollupExpr:
		return getCommonLabelFilters(t.Expr)
	case *FuncExpr:
		if strings.ToLower(t.Name) == "label_set" {
			return getCommonLabelFiltersForLabelSet(t.Args)
		}
		arg := getFuncArgForOptimization(t.Name, t.Args)
		if arg == nil {
			return nil
		}
		return getCommonLabelFilters(arg)
	case *AggrFuncExpr:
		args := t.Args
		if len(args) > 0 && canAcceptMultipleArgsForAggrFunc(t.Name) {
			lfs := getCommonLabelFilters(args[0])
			for _, arg := range args[1:] {
				lfsNext := getCommonLabelFilters(arg)
				lfs = intersectLabelFilters(lfs, lfsNext)
			}
			return trimFiltersByAggrModifier(lfs, t)
		}
		arg := getFuncArgForOptimization(t.Name, args)
		if arg == nil {
			return nil
		}
		lfs := getCommonLabelFilters(arg)
		return trimFiltersByAggrModifier(lfs, t)
	case *BinaryOpExpr:
		lfsLeft := getCommonLabelFilters(t.Left)
		lfsRight := getCommonLabelFilters(t.Right)
		var lfs []LabelFilter
		switch strings.ToLower(t.Op) {
		case "or":
			// {fCommon, f1} or {fCommon, f2} -> {fCommon}
			// {fCommon, f1} or on() {fCommon, f2} -> {}
			// {fCommon, f1} or on(fCommon) {fCommon, f2} -> {fCommon}
			// {fCommon, f1} or on(f1) {fCommon, f2} -> {}
			// {fCommon, f1} or on(f2) {fCommon, f2} -> {}
			// {fCommon, f1} or on(f3) {fCommon, f2} -> {}
			lfs = intersectLabelFilters(lfsLeft, lfsRight)
			return TrimFiltersByGroupModifier(lfs, t)
		case "unless":
			// {f1} unless {f2} -> {f1}
			// {f1} unless on() {f2} -> {}
			// {f1} unless on(f1) {f2} -> {f1}
			// {f1} unless on(f2) {f2} -> {}
			// {f1} unless on(f1, f2) {f2} -> {f1}
			// {f1} unless on(f3) {f2} -> {}
			return TrimFiltersByGroupModifier(lfsLeft, t)
		default:
			switch strings.ToLower(t.JoinModifier.Op) {
			case "group_left":
				// {f1} * group_left() {f2} -> {f1, f2}
				// {f1} * on() group_left() {f2} -> {f1}
				// {f1} * on(f1) group_left() {f2} -> {f1}
				// {f1} * on(f2) group_left() {f2} -> {f1, f2}
				// {f1} * on(f1, f2) group_left() {f2} -> {f1, f2}
				// {f1} * on(f3) group_left() {f2} -> {f1}
				lfsRight = TrimFiltersByGroupModifier(lfsRight, t)
				return unionLabelFilters(lfsLeft, lfsRight)
			case "group_right":
				// {f1} * group_right() {f2} -> {f1, f2}
				// {f1} * on() group_right() {f2} -> {f2}
				// {f1} * on(f1) group_right() {f2} -> {f1, f2}
				// {f1} * on(f2) group_right() {f2} -> {f2}
				// {f1} * on(f1, f2) group_right() {f2} -> {f1, f2}
				// {f1} * on(f3) group_right() {f2} -> {f2}
				lfsLeft = TrimFiltersByGroupModifier(lfsLeft, t)
				return unionLabelFilters(lfsLeft, lfsRight)
			default:
				// {f1} * {f2} -> {f1, f2}
				// {f1} * on() {f2} -> {}
				// {f1} * on(f1) {f2} -> {f1}
				// {f1} * on(f2) {f2} -> {f2}
				// {f1} * on(f1, f2) {f2} -> {f2}
				// {f1} * on(f3} {f2} -> {}
				lfs = unionLabelFilters(lfsLeft, lfsRight)
				return TrimFiltersByGroupModifier(lfs, t)
			}
		}
	default:
		return nil
	}
}

func getCommonLabelFiltersForLabelSet(args []Expr) []LabelFilter {
	if len(args) == 0 {
		return nil
	}
	lfs := getCommonLabelFilters(args[0])
	args = args[1:]
	for i := 0; i < len(args); i += 2 {
		labelName := args[i]
		if i+1 >= len(args) {
			return nil
		}
		labelValue := args[i+1]

		seLabelName, ok := labelName.(*StringExpr)
		if !ok {
			return nil
		}
		seLabelValue, ok := labelValue.(*StringExpr)
		if !ok {
			return nil
		}

		if seLabelName.S == "__name__" {
			continue
		}

		lfsDst := lfs[:0]
		for _, lf := range lfs {
			if lf.Label != seLabelName.S {
				lfsDst = append(lfsDst, lf)
			}
		}
		lfs = append(lfsDst, LabelFilter{
			Label: seLabelName.S,
			Value: seLabelValue.S,
		})
	}
	return lfs
}

func trimFiltersByAggrModifier(lfs []LabelFilter, afe *AggrFuncExpr) []LabelFilter {
	switch strings.ToLower(afe.Modifier.Op) {
	case "by":
		return filterLabelFiltersOn(lfs, afe.Modifier.Args)
	case "without":
		return filterLabelFiltersIgnoring(lfs, afe.Modifier.Args)
	default:
		return nil
	}
}

// TrimFiltersByGroupModifier trims lfs by the specified be.GroupModifier.Op (e.g. on() or ignoring()).
//
// The following cases are possible:
// - It returns lfs as is if be doesn't contain any group modifier
// - It returns only filters specified in on()
// - It drops filters specified inside ignoring()
func TrimFiltersByGroupModifier(lfs []LabelFilter, be *BinaryOpExpr) []LabelFilter {
	switch strings.ToLower(be.GroupModifier.Op) {
	case "on":
		return filterLabelFiltersOn(lfs, be.GroupModifier.Args)
	case "ignoring":
		return filterLabelFiltersIgnoring(lfs, be.GroupModifier.Args)
	default:
		return lfs
	}
}

func getCommonLabelFiltersWithoutMetricName(lfss [][]LabelFilter) []LabelFilter {
	if len(lfss) == 0 {
		return nil
	}
	lfsA := getLabelFiltersWithoutMetricName(lfss[0])
	for _, lfs := range lfss[1:] {
		if len(lfsA) == 0 {
			return nil
		}
		lfsB := getLabelFiltersWithoutMetricName(lfs)
		lfsA = intersectLabelFilters(lfsA, lfsB)
	}
	return lfsA
}

func getLabelFiltersWithoutMetricName(lfs []LabelFilter) []LabelFilter {
	lfsNew := make([]LabelFilter, 0, len(lfs))
	for _, lf := range lfs {
		if lf.Label != "__name__" {
			lfsNew = append(lfsNew, lf)
		}
	}
	return lfsNew
}

// PushdownBinaryOpFilters pushes down the given commonFilters to e if possible.
//
// e must be a part of binary operation - either left or right.
//
// For example, if e contains `foo + sum(bar)` and commonFilters={x="y"},
// then the returned expression will contain `foo{x="y"} + sum(bar)`.
// The `{x="y"}` cannot be pusehd down to `sum(bar)`, since this may change binary operation results.
func PushdownBinaryOpFilters(e Expr, commonFilters []LabelFilter) Expr {
	if len(commonFilters) == 0 {
		// Fast path - nothing to push down.
		return e
	}
	eCopy := Clone(e)
	pushdownBinaryOpFiltersInplace(eCopy, commonFilters)
	return eCopy
}

func pushdownBinaryOpFiltersInplace(e Expr, lfs []LabelFilter) {
	if len(lfs) == 0 {
		return
	}
	switch t := e.(type) {
	case *MetricExpr:
		for i, lfsLocal := range t.LabelFilterss {
			lfsLocal = unionLabelFilters(lfsLocal, lfs)
			sortLabelFilters(lfsLocal)
			t.LabelFilterss[i] = lfsLocal
		}
	case *RollupExpr:
		pushdownBinaryOpFiltersInplace(t.Expr, lfs)
	case *FuncExpr:
		if strings.ToLower(t.Name) == "label_set" && len(t.Args) > 0 {
			arg := t.Args[0]
			lfs = getPushdownLabelFiltersForLabelSet(t.Args[1:], lfs)
			pushdownBinaryOpFiltersInplace(arg, lfs)
		} else {
			arg := getFuncArgForOptimization(t.Name, t.Args)
			if arg != nil {
				pushdownBinaryOpFiltersInplace(arg, lfs)
			}
		}
	case *AggrFuncExpr:
		lfs = trimFiltersByAggrModifier(lfs, t)
		args := t.Args
		if len(args) > 0 && canAcceptMultipleArgsForAggrFunc(t.Name) {
			for _, arg := range args {
				pushdownBinaryOpFiltersInplace(arg, lfs)
			}
		} else {
			arg := getFuncArgForOptimization(t.Name, args)
			if arg != nil {
				pushdownBinaryOpFiltersInplace(arg, lfs)
			}
		}
	case *BinaryOpExpr:
		lfs = TrimFiltersByGroupModifier(lfs, t)
		pushdownBinaryOpFiltersInplace(t.Left, lfs)
		pushdownBinaryOpFiltersInplace(t.Right, lfs)
	}
}

func getPushdownLabelFiltersForLabelSet(args []Expr, lfs []LabelFilter) []LabelFilter {
	m := make(map[string]struct{})
	for i := 0; i < len(args); i += 2 {
		labelName := args[i]
		seLabelName, ok := labelName.(*StringExpr)
		if !ok {
			return nil
		}
		m[seLabelName.S] = struct{}{}
	}

	var lfsDst []LabelFilter
	for _, lf := range lfs {
		if _, ok := m[lf.Label]; !ok {
			lfsDst = append(lfsDst, lf)
		}
	}
	return lfsDst
}

func intersectLabelFilters(lfsA, lfsB []LabelFilter) []LabelFilter {
	if len(lfsA) == 0 || len(lfsB) == 0 {
		return nil
	}
	m := getLabelFiltersMap(lfsA)
	var b []byte
	var lfs []LabelFilter
	for _, lf := range lfsB {
		b = lf.AppendString(b[:0])
		if _, ok := m[string(b)]; ok {
			lfs = append(lfs, lf)
		}
	}
	return lfs
}

func unionLabelFilters(lfsA, lfsB []LabelFilter) []LabelFilter {
	if len(lfsA) == 0 {
		return lfsB
	}
	if len(lfsB) == 0 {
		return lfsA
	}
	m := getLabelFiltersMap(lfsA)
	var b []byte
	lfs := append([]LabelFilter{}, lfsA...)
	for _, lf := range lfsB {
		b = lf.AppendString(b[:0])
		if _, ok := m[string(b)]; !ok {
			lfs = append(lfs, lf)
		}
	}
	return lfs
}

func getLabelFiltersMap(lfs []LabelFilter) map[string]struct{} {
	m := make(map[string]struct{}, len(lfs))
	var b []byte
	for _, lf := range lfs {
		b = lf.AppendString(b[:0])
		m[string(b)] = struct{}{}
	}
	return m
}

func sortLabelFilters(lfs []LabelFilter) {
	// Make sure the first label filter is __name__ (if any)
	if len(lfs) > 0 && lfs[0].isMetricNameFilter() {
		lfs = lfs[1:]
	}
	sort.Slice(lfs, func(i, j int) bool {
		a, b := lfs[i], lfs[j]
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Value < b.Value
	})
}

func filterLabelFiltersOn(lfs []LabelFilter, args []string) []LabelFilter {
	if len(args) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(args))
	for _, arg := range args {
		m[arg] = struct{}{}
	}
	var lfsNew []LabelFilter
	for _, lf := range lfs {
		if _, ok := m[lf.Label]; ok {
			lfsNew = append(lfsNew, lf)
		}
	}
	return lfsNew
}

func filterLabelFiltersIgnoring(lfs []LabelFilter, args []string) []LabelFilter {
	if len(args) == 0 {
		return lfs
	}
	m := make(map[string]struct{}, len(args))
	for _, arg := range args {
		m[arg] = struct{}{}
	}
	var lfsNew []LabelFilter
	for _, lf := range lfs {
		if _, ok := m[lf.Label]; !ok {
			lfsNew = append(lfsNew, lf)
		}
	}
	return lfsNew
}

func getFuncArgForOptimization(funcName string, args []Expr) Expr {
	idx := getFuncArgIdxForOptimization(funcName, args)
	if idx < 0 || idx >= len(args) {
		return nil
	}
	return args[idx]
}

func getFuncArgIdxForOptimization(funcName string, args []Expr) int {
	funcName = strings.ToLower(funcName)
	if IsRollupFunc(funcName) {
		return getRollupArgIdxForOptimization(funcName, args)
	}
	if IsTransformFunc(funcName) {
		return getTransformArgIdxForOptimization(funcName, args)
	}
	if IsAggrFunc(funcName) {
		return getAggrArgIdxForOptimization(funcName, args)
	}
	return -1
}

func getAggrArgIdxForOptimization(funcName string, args []Expr) int {
	switch strings.ToLower(funcName) {
	case "bottomk", "bottomk_avg", "bottomk_max", "bottomk_median", "bottomk_last", "bottomk_min",
		"limitk", "outliers_mad", "outliersk", "quantile",
		"topk", "topk_avg", "topk_max", "topk_median", "topk_last", "topk_min":
		return 1
	case "count_values":
		return -1
	case "quantiles":
		return len(args) - 1
	default:
		if len(args) > 1 && canAcceptMultipleArgsForAggrFunc(funcName) {
			panic(fmt.Errorf("BUG: %d > 1 args passed to aggregate function %q; this case must be already handled", len(args), funcName))
		}
		return 0
	}
}

func canAcceptMultipleArgsForAggrFunc(funcName string) bool {
	switch strings.ToLower(funcName) {
	case "any", "avg", "count", "distinct", "geomean", "group", "histogram", "mad", "max",
		"median", "min", "mode", "share", "stddev", "stdvar", "sum", "sum2", "zscore":
		return true
	default:
		return false
	}
}

func getRollupArgIdxForOptimization(funcName string, args []Expr) int {
	// This must be kept in sync with GetRollupArgIdx()
	switch strings.ToLower(funcName) {
	case "absent_over_time":
		return -1
	case "quantile_over_time", "aggr_over_time",
		"hoeffding_bound_lower", "hoeffding_bound_upper":
		return 1
	case "quantiles_over_time":
		return len(args) - 1
	default:
		return 0
	}
}

func getTransformArgIdxForOptimization(funcName string, args []Expr) int {
	switch strings.ToLower(funcName) {
	case "", "absent", "scalar", "union", "vector", "range_normalize":
		return -1
	case "end", "now", "pi", "ru", "start", "step", "time":
		return -1
	case "limit_offset":
		return 2
	case "buckets_limit", "histogram_quantile", "histogram_share", "range_quantile",
		"range_trim_outliers", "range_trim_spikes", "range_trim_zscore":
		return 1
	case "histogram_quantiles":
		return len(args) - 1
	case "drop_common_labels", "label_copy", "label_del", "label_graphite_group", "label_join", "label_keep", "label_lowercase",
		"label_map", "label_move", "label_replace", "label_set", "label_transform", "label_uppercase":
		return -1
	default:
		return 0
	}
}
