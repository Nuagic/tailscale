// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package clientmetric provides client-side metrics whose values
// get occasionally logged.
package clientmetric

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	mu          sync.Mutex // guards vars in this block
	metrics     = map[string]*Metric{}
	numWireID   int       // how many wireIDs have been allocated
	lastDelta   time.Time // time of last call to EncodeLogTailMetricsDelta
	sortedDirty bool      // whether sorted needs to be rebuilt
	sorted      []*Metric // by name
)

// Type is a metric type: counter or gauge.
type Type uint8

const (
	TypeGauge Type = iota
	TypeCounter
)

// Metric is an integer metric value that's tracked over time.
//
// It's safe for concurrent use.
type Metric struct {
	v    int64 // atomic; the metric value
	name string
	typ  Type

	// Owned by package-level 'mu'.
	wireID     int // zero until named
	lastNamed  time.Time
	lastLogVal int64
}

func (m *Metric) Name() string { return m.name }
func (m *Metric) Value() int64 { return atomic.LoadInt64(&m.v) }
func (m *Metric) Type() Type   { return m.typ }

// Add increments m's value by n.
//
// If m is of type counter, n should not be negative.
func (m *Metric) Add(n int64) {
	atomic.AddInt64(&m.v, n)
}

// Set sets m's value to v.
//
// If m is of type counter, Set should not be used.
func (m *Metric) Set(v int64) {
	atomic.StoreInt64(&m.v, v)
}

// Publish registers a metric in the global map.
// It panics if the name is a duplicate anywhere in the process.
func (m *Metric) Publish() {
	mu.Lock()
	defer mu.Unlock()
	if m.name == "" {
		panic("unnamed Metric")
	}
	if _, dup := metrics[m.name]; dup {
		panic("duplicate metric " + m.name)
	}
	metrics[m.name] = m
	sortedDirty = true
}

// Metrics returns the sorted list of metrics.
//
// The returned slice should not be mutated.
func Metrics() []*Metric {
	mu.Lock()
	defer mu.Unlock()
	if sortedDirty {
		sortedDirty = false
		sorted = make([]*Metric, 0, len(metrics))
		for _, m := range metrics {
			sorted = append(sorted, m)
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].name < sorted[j].name
		})
	}
	return sorted
}

// NewUnpublished initializes a new Metric without calling Publish on
// it.
func NewUnpublished(name string, typ Type) *Metric {
	if i := strings.IndexFunc(name, isIllegalMetricRune); name == "" || i != -1 {
		panic(fmt.Sprintf("illegal metric name %q (index %v)", name, i))
	}
	return &Metric{
		name: name,
		typ:  typ,
	}
}

func isIllegalMetricRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' ||
		r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' ||
		r == '_')
}

// NewCounter returns a new metric that can only increment.
func NewCounter(name string) *Metric {
	m := NewUnpublished(name, TypeCounter)
	m.Publish()
	return m
}

// NewGauge returns a new metric that can both increment and decrement.
func NewGauge(name string) *Metric {
	m := NewUnpublished(name, TypeGauge)
	m.Publish()
	return m
}

// WritePrometheusExpositionFormat writes all client metrics to w in
// the Prometheus text-based exposition format.
//
// See https://github.com/prometheus/docs/blob/main/content/docs/instrumenting/exposition_formats.md
func WritePrometheusExpositionFormat(w io.Writer) {
	for _, m := range Metrics() {
		switch m.Type() {
		case TypeGauge:
			fmt.Fprintf(w, "# TYPE %s gauge\n", m.Name())
		case TypeCounter:
			fmt.Fprintf(w, "# TYPE %s counter\n", m.Name())
		}
		fmt.Fprintf(w, "%s %v\n", m.Name(), m.Value())
	}
}

const (
	// metricLogNameFrequency is how often a metric's name=>id
	// mapping is redundantly put in the logs. In other words,
	// this is how how far in the logs you need to fetch from a
	// given point in time to recompute the metrics at that point
	// in time.
	metricLogNameFrequency = 4 * time.Hour

	// minMetricEncodeInterval is the minimum interval that the
	// metrics will be scanned for changes before being encoded
	// for logtail.
	minMetricEncodeInterval = 15 * time.Second
)

// EncodeLogTailMetricsDelta return an encoded string representing the metrics
// differences since the previous call.
//
// It implements the requirements of a logtail.Config.MetricsDelta
// func. Notably, its output is safe to embed in a JSON string literal
// without further escaping.
//
// The current encoding is:
//   * name immediately following metric:
//     'N' + hex(varint(len(name))) + name
//   * set value of a metric:
//     'S' + hex(varint(wireid)) + hex(varint(value))
//   * increment a metric: (decrements if negative)
//     'I' + hex(varint(wireid)) + hex(varint(value))
func EncodeLogTailMetricsDelta() string {
	mu.Lock()
	defer mu.Unlock()

	now := time.Now()
	if !lastDelta.IsZero() && now.Sub(lastDelta) < minMetricEncodeInterval {
		return ""
	}
	lastDelta = now

	var enc *deltaEncBuf // lazy
	for _, m := range metrics {
		val := m.Value()
		delta := val - m.lastLogVal
		if delta == 0 {
			continue
		}
		if enc == nil {
			enc = deltaPool.Get().(*deltaEncBuf)
			enc.buf.Reset()
		}
		m.lastLogVal = val
		if m.wireID == 0 {
			numWireID++
			m.wireID = numWireID
		}
		if m.lastNamed.IsZero() || now.Sub(m.lastNamed) > metricLogNameFrequency {
			enc.writeName(m.Name())
			m.lastNamed = now
			enc.writeValue(m.wireID, val)
		} else {
			enc.writeDelta(m.wireID, delta)
		}
	}
	if enc == nil {
		return ""
	}
	defer deltaPool.Put(enc)
	return enc.buf.String()
}

var deltaPool = &sync.Pool{
	New: func() interface{} {
		return new(deltaEncBuf)
	},
}

// deltaEncBuf encodes metrics per the format described
// on EncodeLogTailMetricsDelta above.
type deltaEncBuf struct {
	buf     bytes.Buffer
	scratch [binary.MaxVarintLen64]byte
}

// writeName writes a "name" (N) record to the buffer, which notes
// that the immediately following record's wireID has the provided
// name.
func (b *deltaEncBuf) writeName(name string) {
	b.buf.WriteByte('N')
	b.writeHexVarint(int64(len(name)))
	b.buf.WriteString(name)
}

// writeDelta writes a "set" (S) record to the buffer, noting that the
// metric with the given wireID now has value v.
func (b *deltaEncBuf) writeValue(wireID int, v int64) {
	b.buf.WriteByte('S')
	b.writeHexVarint(int64(wireID))
	b.writeHexVarint(v)
}

// writeDelta writes an "increment" (I) delta value record to the
// buffer, noting that the metric with the given wireID now has a
// value that's v larger (or smaller if v is negative).
func (b *deltaEncBuf) writeDelta(wireID int, v int64) {
	b.buf.WriteByte('I')
	b.writeHexVarint(int64(wireID))
	b.writeHexVarint(v)
}

// writeHexVarint writes v to the buffer as a hex-encoded varint.
func (b *deltaEncBuf) writeHexVarint(v int64) {
	n := binary.PutVarint(b.scratch[:], v)
	hexLen := n * 2
	oldLen := b.buf.Len()
	b.buf.Grow(hexLen)
	hexBuf := b.buf.Bytes()[oldLen : oldLen+hexLen]
	hex.Encode(hexBuf, b.scratch[:n])
	b.buf.Write(hexBuf)
}
