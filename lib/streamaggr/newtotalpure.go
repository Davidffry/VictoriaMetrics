package streamaggr

import (
	"math"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
)

// newotalPureAggrState calculates output=newtotal, e.g. the summary counter over input counters.
type newotalPureAggrState struct {
	m                   sync.Map
	intervalSecs        uint64
	ignoreInputDeadline uint64
	stalenessSecs       uint64
	lastPushTimestamp   uint64
}

type newtotalPureStateValue struct {
	mu             sync.Mutex
	lastValues     map[string]*lastValueState
	total          float64
	samplesCount   uint64
	deleteDeadline uint64
	deleted        bool
}

func newnewotalPureAggrState(interval time.Duration, stalenessInterval time.Duration) *newotalPureAggrState {
	currentTime := fasttime.UnixTimestamp()
	intervalSecs := roundDurationToSecs(interval)
	stalenessSecs := roundDurationToSecs(stalenessInterval)
	return &newotalPureAggrState{
		intervalSecs:        intervalSecs,
		stalenessSecs:       stalenessSecs,
		ignoreInputDeadline: currentTime + intervalSecs,
	}
}

func (as *newotalPureAggrState) pushSample(inputKey, outputKey string, value float64) {
	currentTime := fasttime.UnixTimestamp()
	deleteDeadline := currentTime + as.stalenessSecs

again:
	v, ok := as.m.Load(outputKey)
	if !ok {
		// The entry is missing in the map. Try creating it.
		v = &newtotalPureStateValue{
			lastValues: make(map[string]*lastValueState),
		}
		vNew, loaded := as.m.LoadOrStore(outputKey, v)
		if loaded {
			// Use the entry created by a concurrent goroutine.
			v = vNew
		}
	}
	sv := v.(*newtotalPureStateValue)
	sv.mu.Lock()
	deleted := sv.deleted
	if !deleted {
		lv, ok := sv.lastValues[inputKey]
		if !ok {
			lv = &lastValueState{}
			lv.firstValue = value
			lv.value = value
			lv.correction = 0
			sv.lastValues[inputKey] = lv
		}

		// process counter reset
		delta := value - lv.value
		if delta < 0 {
			if (-delta * 8) < lv.value {
				lv.correction += lv.value - value
			} else {
				lv.correction += lv.value
			}
		}

		// process increasing counter
		correctedValue := value + lv.correction
		correctedDelta := correctedValue - lv.firstValue
		if ok && math.Abs(correctedValue) < 10*(math.Abs(correctedDelta)+1) {
			correctedDelta = correctedValue
		}
		sv.total = correctedDelta
		lv.value = value
		lv.deleteDeadline = deleteDeadline
		sv.deleteDeadline = deleteDeadline
		sv.samplesCount++
	}
	sv.mu.Unlock()
	if deleted {
		// The entry has been deleted by the concurrent call to appendSeriesForFlush
		// Try obtaining and updating the entry again.
		goto again
	}
}

func (as *newotalPureAggrState) removeOldEntries(currentTime uint64) {
	m := &as.m
	m.Range(func(k, v interface{}) bool {
		sv := v.(*newtotalPureStateValue)

		sv.mu.Lock()
		deleted := currentTime > sv.deleteDeadline
		if deleted {
			// Mark the current entry as deleted
			sv.deleted = deleted
		} else {
			// Delete outdated entries in sv.lastValues
			m := sv.lastValues
			for k1, v1 := range m {
				if currentTime > v1.deleteDeadline {
					delete(m, k1)
				}
			}
		}
		sv.mu.Unlock()

		if deleted {
			m.Delete(k)
		}
		return true
	})
}

func (as *newotalPureAggrState) appendSeriesForFlush(ctx *flushCtx) {
	currentTime := fasttime.UnixTimestamp()
	currentTimeMsec := int64(currentTime) * 1000

	as.removeOldEntries(currentTime)

	m := &as.m
	m.Range(func(k, v interface{}) bool {
		sv := v.(*newtotalPureStateValue)
		sv.mu.Lock()
		total := sv.total
		if math.Abs(sv.total) >= (1 << 53) {
			// It is time to reset the entry, since it starts losing float64 precision
			sv.total = 0
		}
		deleted := sv.deleted
		sv.mu.Unlock()
		if !deleted {
			key := k.(string)
			ctx.appendSeries(key, as.getOutputName(), currentTimeMsec, total)
		}
		return true
	})
	as.lastPushTimestamp = currentTime
}

func (as *newotalPureAggrState) getOutputName() string {
	return "newtotal_pure"
}

func (as *newotalPureAggrState) getStateRepresentation(suffix string) []aggrStateRepresentation {
	result := make([]aggrStateRepresentation, 0)
	as.m.Range(func(k, v any) bool {
		value := v.(*newtotalPureStateValue)
		value.mu.Lock()
		defer value.mu.Unlock()
		if value.deleted {
			return true
		}
		result = append(result, aggrStateRepresentation{
			metric:            getLabelsStringFromKey(k.(string), suffix, as.getOutputName()),
			currentValue:      value.total,
			lastPushTimestamp: as.lastPushTimestamp,
			nextPushTimestamp: as.lastPushTimestamp + as.intervalSecs,
			samplesCount:      value.samplesCount,
		})
		return true
	})
	return result
}
