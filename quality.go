package main

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Stream quality for the Stats tab: why streams transcode, how often each
// device plays directly, hardware against software transcoding, the
// resolution changes transcodes make, and bandwidth over time. Unlike the
// play counts elsewhere, these count every stream (a resumed play that
// switched from direct play to a transcode is two streams), because quality
// belongs to the stream rather than to the viewing.

type qualitySummary struct {
	Reasons           []rankedName      `json:"reasons"`
	Devices           []deviceQuality   `json:"devices"`
	HardwareTranscode int               `json:"hardware_transcodes"`
	SoftwareTranscode int               `json:"software_transcodes"`
	Resolutions       []rankedName      `json:"resolution_changes"`
	Bandwidth         []bandwidthBucket `json:"bandwidth,omitempty"`
	PeakBandwidth     *bandwidthPeak    `json:"peak_bandwidth,omitempty"`
}

type deviceQuality struct {
	Name         string `json:"name"`
	Plays        int    `json:"plays"`
	DirectPlay   int    `json:"direct_play"`
	DirectStream int    `json:"direct_stream"`
	Transcode    int    `json:"transcode"`
}

type bandwidthBucket struct {
	Start time.Time `json:"t"`
	// Busiest minute in the bucket, and the average over the minutes with
	// streams, local and remote.
	PeakKbps int64 `json:"peak_kbps"`
	LANKbps  int64 `json:"lan_kbps"`
	WANKbps  int64 `json:"wan_kbps"`
}

type bandwidthPeak struct {
	Kbps int64     `json:"kbps"`
	At   time.Time `json:"at"`
}

const qualityListLimit = 8

func streamQuality(plays []statsPlay) qualitySummary {
	result := qualitySummary{Reasons: []rankedName{}, Devices: []deviceQuality{}, Resolutions: []rankedName{}}
	reasons := map[string]int{}
	resolutions := map[string]int{}
	devices := map[string]*deviceQuality{}
	for _, play := range plays {
		name := firstNonEmpty(play.client, displayPlatform(play.platform))
		if name != "" {
			device := devices[name]
			if device == nil {
				device = &deviceQuality{Name: name}
				devices[name] = device
			}
			device.Plays++
			switch play.method {
			case "direct_play":
				device.DirectPlay++
			case "direct_stream":
				device.DirectStream++
			case "transcode":
				device.Transcode++
			}
		}
		if play.method != "transcode" {
			continue
		}
		for _, code := range strings.Split(play.reasons, ",") {
			if label := reasonLabel(code); label != "" {
				reasons[label]++
			}
		}
		if hardwareEncode(play.hwEncode) {
			result.HardwareTranscode++
		} else {
			result.SoftwareTranscode++
		}
		from, to := resolutionLabel(play.sourceRes), resolutionLabel(play.streamRes)
		if from != "" && to != "" && from != to {
			resolutions[from+" → "+to]++
		}
	}
	result.Reasons = topNames(reasons)
	result.Resolutions = topNames(resolutions)
	for _, device := range devices {
		result.Devices = append(result.Devices, *device)
	}
	sort.Slice(result.Devices, func(i, j int) bool {
		if result.Devices[i].Plays != result.Devices[j].Plays {
			return result.Devices[i].Plays > result.Devices[j].Plays
		}
		return result.Devices[i].Name < result.Devices[j].Name
	})
	if len(result.Devices) > qualityListLimit {
		result.Devices = result.Devices[:qualityListLimit]
	}
	return result
}

func topNames(counts map[string]int) []rankedName {
	names := make([]rankedName, 0, len(counts))
	for name, plays := range counts {
		names = append(names, rankedName{Name: name, Plays: plays})
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].Plays != names[j].Plays {
			return names[i].Plays > names[j].Plays
		}
		return names[i].Name < names[j].Name
	})
	if len(names) > qualityListLimit {
		names = names[:qualityListLimit]
	}
	return names
}

// reasonUpper lists words that read as acronyms in a reason.
var reasonUpper = map[string]string{"hdr": "HDR", "sdr": "SDR", "hls": "HLS", "aac": "AAC", "hevc": "HEVC", "h264": "H.264", "h265": "H.265", "av1": "AV1", "vp9": "VP9", "ac3": "AC3", "eac3": "E-AC3", "dts": "DTS", "pgs": "PGS", "srt": "SRT", "ass": "ASS", "4k": "4K"}

// reasonLabel turns a reason code into words: Silo's adaptation reasons
// ("video_codec_unsupported") and the ones Plex ingest derives
// ("video_transcode") alike. Codes Siloscope doesn't know still read well.
func reasonLabel(code string) string {
	words := strings.Fields(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(code)), "_", " "))
	if len(words) == 0 {
		return ""
	}
	for i, word := range words {
		if upper, ok := reasonUpper[word]; ok {
			words[i] = upper
		}
	}
	if first := words[0]; first == strings.ToLower(first) {
		words[0] = strings.ToUpper(first[:1]) + first[1:]
	}
	return strings.Join(words, " ")
}

// hardwareEncode reports whether a transcode's encoder value names hardware:
// Silo's transcode_hw_accel ("vaapi", "nvenc", "qsv"...) or Plex's encoder.
func hardwareEncode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "software", "sw", "false", "0", "cpu", "libx264", "libx265":
		return false
	}
	return true
}

func resolutionLabel(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "":
		return ""
	case "4k", "2160", "2160p", "uhd":
		return "4K"
	case "sd", "480", "480p", "576", "576p":
		return "SD"
	}
	if !strings.HasSuffix(strings.ToLower(value), "p") && strings.Trim(value, "0123456789") == "" {
		return value + "p"
	}
	return value
}

// bandwidthSeries reads the per-minute stream samples into the stats range's
// buckets (the same ones as plays over time), with the busiest minute.
func (app *application) bandwidthSeries(ctx context.Context, r statsRange, source string, over []statsBucket) ([]bandwidthBucket, *bandwidthPeak) {
	if len(over) == 0 {
		return nil, nil
	}
	query := `SELECT at, SUM(bandwidth_kbps), SUM(lan_kbps), SUM(wan_kbps) FROM stream_samples WHERE at >= ?`
	args := []any{over[0].Start.UnixMilli()}
	if source == "silo" || source == "plex" {
		query += ` AND source = ?`
		args = append(args, source)
	}
	rows, err := app.store.db.QueryContext(ctx, query+` GROUP BY at ORDER BY at`, args...)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	type sums struct {
		peak, lan, wan int64
		minutes        int64
	}
	byBucket := map[int64]*sums{}
	var peak *bandwidthPeak
	for rows.Next() {
		var at, total, lan, wan int64
		if rows.Scan(&at, &total, &lan, &wan) != nil {
			return nil, nil
		}
		when := time.UnixMilli(at)
		key := r.bucketStart(when).Unix()
		bucket := byBucket[key]
		if bucket == nil {
			bucket = &sums{}
			byBucket[key] = bucket
		}
		bucket.peak = max(bucket.peak, total)
		if total > 0 {
			bucket.lan += lan
			bucket.wan += wan
			bucket.minutes++
		}
		if total > 0 && (peak == nil || total > peak.Kbps) {
			peak = &bandwidthPeak{Kbps: total, At: when.In(r.location)}
		}
	}
	if rows.Err() != nil {
		return nil, nil
	}
	series := make([]bandwidthBucket, 0, len(over))
	for _, point := range over {
		bucket := bandwidthBucket{Start: point.Start}
		if sums := byBucket[point.Start.Unix()]; sums != nil {
			bucket.PeakKbps = sums.peak
			if sums.minutes > 0 {
				bucket.LANKbps = sums.lan / sums.minutes
				bucket.WANKbps = sums.wan / sums.minutes
			}
		}
		series = append(series, bucket)
	}
	return series, peak
}
