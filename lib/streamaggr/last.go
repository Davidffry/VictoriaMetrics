package streamaggr

import (
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
)

// lastAggrState calculates output=last, e.g. the last value over input samples.
type lastAggrState struct {
	m                 sync.Map
	intervalSecs      uint64
	stalenessSecs     uint64
	lastPushTimestamp uint64
}

type lastStateValue struct {
	mu             sync.Mutex
	last           float64
	samplesCount   uint64
	deleted        bool
	deleteDeadline uint64
}

func newLastAggrState(interval time.Duration, stalenessInterval time.Duration) *lastAggrState {
	return &lastAggrState{
		intervalSecs:  roundDurationToSecs(interval),
		stalenessSecs: roundDurationToSecs(stalenessInterval),
	}
}

func (as *lastAggrState) pushSample(_, outputKey string, value float64) {
	currentTime := fasttime.UnixTimestamp()
	deleteDeadline := currentTime + as.stalenessSecs

again:
	v, ok := as.m.Load(outputKey)
	if !ok {
		// The entry is missing in the map. Try creating it.
		v = &lastStateValue{
			last: value,
		}
		vNew, loaded := as.m.LoadOrStore(outputKey, v)
		if !loaded {
			// The new entry has been successfully created.
			return
		}
		// Use the entry created by a concurrent goroutine.
		v = vNew
	}
	sv := v.(*lastStateValue)
	sv.mu.Lock()
	deleted := sv.deleted
	if !deleted {
		sv.last = value
		sv.samplesCount++
		sv.deleteDeadline = deleteDeadline
	}
	sv.mu.Unlock()
	if deleted {
		// The entry has been deleted by the concurrent call to appendSeriesForFlush
		// Try obtaining and updating the entry again.
		goto again
	}
}

func (as *lastAggrState) removeOldEntries(currentTime uint64) {
	m := &as.m
	m.Range(func(k, v interface{}) bool {
		sv := v.(*lastStateValue)

		sv.mu.Lock()
		deleted := currentTime > sv.deleteDeadline
		if deleted {
			// Mark the current entry as deleted
			sv.deleted = deleted
		}
		sv.mu.Unlock()

		if deleted {
			m.Delete(k)
		}
		return true
	})
}

func (as *lastAggrState) appendSeriesForFlush(ctx *flushCtx) {
	currentTime := fasttime.UnixTimestamp()
	currentTimeMsec := int64(currentTime) * 1000

	as.removeOldEntries(currentTime)

	m := &as.m
	m.Range(func(k, v interface{}) bool {
		sv := v.(*lastStateValue)
		sv.mu.Lock()
		last := sv.last
		sv.mu.Unlock()
		key := k.(string)
		ctx.appendSeries(key, as.getOutputName(), currentTimeMsec, last)
		return true
	})
	as.lastPushTimestamp = currentTime
}

func (as *lastAggrState) getOutputName() string {
	return "last"
}

func (as *lastAggrState) getStateRepresentation(suffix string) []aggrStateRepresentation {
	result := make([]aggrStateRepresentation, 0)
	as.m.Range(func(k, v any) bool {
		value := v.(*lastStateValue)
		value.mu.Lock()
		defer value.mu.Unlock()
		if value.deleted {
			return true
		}
		result = append(result, aggrStateRepresentation{
			metric:            getLabelsStringFromKey(k.(string), suffix, as.getOutputName()),
			currentValue:      value.last,
			lastPushTimestamp: as.lastPushTimestamp,
			nextPushTimestamp: as.lastPushTimestamp + as.intervalSecs,
			samplesCount:      value.samplesCount,
		})
		return true
	})
	return result
}
