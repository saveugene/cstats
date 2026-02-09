package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
)

// Same colorblind-friendly palette as plot.py.
var colors = []string{
	"#636EFA", "#EF553B", "#00CC96", "#AB63FA", "#FFA15A",
	"#19D3F3", "#FF6692", "#B6E880", "#FF97FF", "#FECB52",
}

type record struct {
	Timestamp  time.Time
	Container  string
	CPUPct     float64
	MemUsageMB float64
	MemLimitMB float64
	MemPct     float64
}

type containerStats struct {
	CPUMax    float64
	CPUSum    float64
	MemMax    float64
	MemSum    float64
	MemPctMax float64
	Count     int
}

// loadCSV reads and parses the CSV file.
func loadCSV(path string) ([]record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}

	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	need := []string{"timestamp", "container", "cpu_pct", "mem_usage_mb", "mem_limit_mb", "mem_pct"}
	for _, n := range need {
		if _, ok := idx[n]; !ok {
			return nil, fmt.Errorf("missing column %q", n)
		}
	}

	var records []record
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, strings.TrimSpace(row[idx["timestamp"]]))
		if err != nil {
			ts, err = time.Parse("2006-01-02T15:04:05Z", strings.TrimSpace(row[idx["timestamp"]]))
			if err != nil {
				continue
			}
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSpace(row[idx["cpu_pct"]]), 64)
		memU, _ := strconv.ParseFloat(strings.TrimSpace(row[idx["mem_usage_mb"]]), 64)
		memL, _ := strconv.ParseFloat(strings.TrimSpace(row[idx["mem_limit_mb"]]), 64)
		memP, _ := strconv.ParseFloat(strings.TrimSpace(row[idx["mem_pct"]]), 64)

		records = append(records, record{
			Timestamp:  ts,
			Container:  strings.TrimSpace(row[idx["container"]]),
			CPUPct:     cpu,
			MemUsageMB: memU,
			MemLimitMB: memL,
			MemPct:     memP,
		})
	}
	return records, nil
}

// buildFigure constructs a Plotly figure JSON matching plot.py's layout.
func buildFigure(records []record) map[string]any {
	if len(records) == 0 {
		return emptyFigure()
	}

	// Collect sorted unique container names.
	seen := map[string]bool{}
	for _, r := range records {
		seen[r.Container] = true
	}
	containers := make([]string, 0, len(seen))
	for c := range seen {
		containers = append(containers, c)
	}
	sort.Strings(containers)

	colorMap := make(map[string]string, len(containers))
	for i, c := range containers {
		colorMap[c] = colors[i%len(colors)]
	}

	// Group records by container, sorted by timestamp.
	grouped := map[string][]record{}
	for _, r := range records {
		grouped[r.Container] = append(grouped[r.Container], r)
	}
	for _, recs := range grouped {
		sort.Slice(recs, func(i, j int) bool {
			return recs[i].Timestamp.Before(recs[j].Timestamp)
		})
	}

	// Summary stats per container.
	stats := map[string]*containerStats{}
	for _, r := range records {
		s, ok := stats[r.Container]
		if !ok {
			s = &containerStats{}
			stats[r.Container] = s
		}
		s.CPUSum += r.CPUPct
		if r.CPUPct > s.CPUMax {
			s.CPUMax = r.CPUPct
		}
		s.MemSum += r.MemUsageMB
		if r.MemUsageMB > s.MemMax {
			s.MemMax = r.MemUsageMB
		}
		if r.MemPct > s.MemPctMax {
			s.MemPctMax = r.MemPct
		}
		s.Count++
	}

	var traces []map[string]any

	// Subplot axes mapping:
	// row1col1: x,y (CPU time series)     row1col2: x2,y2 (CPU bars)
	// row2col1: x3,y3 (RAM time series)   row2col2: x4,y4 (RAM bars)
	// row3col1: x5,y5 (Mem% time series)  row3col2: table (no axes)

	// Time series traces for each container.
	for _, name := range containers {
		recs := grouped[name]
		color := colorMap[name]
		timestamps := make([]string, len(recs))
		cpuVals := make([]float64, len(recs))
		memVals := make([]float64, len(recs))
		memPctVals := make([]float64, len(recs))
		for i, r := range recs {
			timestamps[i] = r.Timestamp.Format(time.RFC3339)
			cpuVals[i] = r.CPUPct
			memVals[i] = r.MemUsageMB
			memPctVals[i] = r.MemPct
		}

		// CPU % time series (row1, col1)
		traces = append(traces, map[string]any{
			"type":        "scatter",
			"x":           timestamps,
			"y":           cpuVals,
			"name":        name,
			"legendgroup": name,
			"showlegend":  true,
			"mode":        "lines+markers",
			"marker":      map[string]any{"size": 3},
			"line":        map[string]any{"color": color, "width": 1.5},
			"hovertemplate": "%{x|%H:%M:%S}<br>CPU: %{y:.1f}%<extra>" + name + "</extra>",
			"xaxis":        "x",
			"yaxis":        "y",
		})

		// RAM time series (row2, col1)
		traces = append(traces, map[string]any{
			"type":        "scatter",
			"x":           timestamps,
			"y":           memVals,
			"name":        name,
			"legendgroup": name,
			"showlegend":  false,
			"mode":        "lines+markers",
			"marker":      map[string]any{"size": 3},
			"line":        map[string]any{"color": color, "width": 1.5},
			"hovertemplate": "%{x|%H:%M:%S}<br>RAM: %{y:.1f} MB<extra>" + name + "</extra>",
			"xaxis":        "x3",
			"yaxis":        "y3",
		})

		// Mem % time series (row3, col1)
		traces = append(traces, map[string]any{
			"type":        "scatter",
			"x":           timestamps,
			"y":           memPctVals,
			"name":        name,
			"legendgroup": name,
			"showlegend":  false,
			"mode":        "lines+markers",
			"marker":      map[string]any{"size": 3},
			"line":        map[string]any{"color": color, "width": 1.5},
			"hovertemplate": "%{x|%H:%M:%S}<br>Mem: %{y:.2f}%<extra>" + name + "</extra>",
			"xaxis":        "x5",
			"yaxis":        "y5",
		})
	}

	// Bar chart data.
	cpuMaxVals := make([]float64, len(containers))
	cpuAvgVals := make([]float64, len(containers))
	memMaxVals := make([]float64, len(containers))
	memAvgVals := make([]float64, len(containers))
	for i, c := range containers {
		s := stats[c]
		cpuMaxVals[i] = round1(s.CPUMax)
		cpuAvgVals[i] = round1(s.CPUSum / float64(s.Count))
		memMaxVals[i] = round1(s.MemMax)
		memAvgVals[i] = round1(s.MemSum / float64(s.Count))
	}

	// CPU bar - peak (row1, col2)
	traces = append(traces, map[string]any{
		"type":          "bar",
		"x":             containers,
		"y":             cpuMaxVals,
		"name":          "peak",
		"marker":        map[string]any{"color": "rgba(239,85,59,0.7)"},
		"showlegend":    false,
		"hovertemplate": "%{x}<br>Peak CPU: %{y:.1f}%<extra></extra>",
		"xaxis":         "x2",
		"yaxis":         "y2",
	})
	// CPU bar - avg (row1, col2)
	traces = append(traces, map[string]any{
		"type":          "bar",
		"x":             containers,
		"y":             cpuAvgVals,
		"name":          "avg",
		"marker":        map[string]any{"color": "rgba(99,110,250,0.7)"},
		"showlegend":    false,
		"hovertemplate": "%{x}<br>Avg CPU: %{y:.1f}%<extra></extra>",
		"xaxis":         "x2",
		"yaxis":         "y2",
	})
	// RAM bar - peak (row2, col2)
	traces = append(traces, map[string]any{
		"type":          "bar",
		"x":             containers,
		"y":             memMaxVals,
		"name":          "peak",
		"marker":        map[string]any{"color": "rgba(239,85,59,0.7)"},
		"showlegend":    false,
		"hovertemplate": "%{x}<br>Peak RAM: %{y:.1f} MB<extra></extra>",
		"xaxis":         "x4",
		"yaxis":         "y4",
	})
	// RAM bar - avg (row2, col2)
	traces = append(traces, map[string]any{
		"type":          "bar",
		"x":             containers,
		"y":             memAvgVals,
		"name":          "avg",
		"marker":        map[string]any{"color": "rgba(99,110,250,0.7)"},
		"showlegend":    false,
		"hovertemplate": "%{x}<br>Avg RAM: %{y:.1f} MB<extra></extra>",
		"xaxis":         "x4",
		"yaxis":         "y4",
	})

	// Summary table (row3, col2).
	tContainers := make([]string, len(containers))
	tCPUAvg := make([]float64, len(containers))
	tCPUMax := make([]float64, len(containers))
	tMemAvg := make([]float64, len(containers))
	tMemMax := make([]float64, len(containers))
	tMemPctMax := make([]float64, len(containers))
	for i, c := range containers {
		s := stats[c]
		tContainers[i] = c
		tCPUAvg[i] = round1(s.CPUSum / float64(s.Count))
		tCPUMax[i] = round1(s.CPUMax)
		tMemAvg[i] = round1(s.MemSum / float64(s.Count))
		tMemMax[i] = round1(s.MemMax)
		tMemPctMax[i] = round2(s.MemPctMax)
	}
	traces = append(traces, map[string]any{
		"type": "table",
		"header": map[string]any{
			"values":     []string{"Container", "CPU avg%", "CPU max%", "RAM avg MB", "RAM max MB", "Mem max%"},
			"fill":       map[string]any{"color": "#2a2a2a"},
			"font":       map[string]any{"color": "white", "size": 11},
			"align":      "left",
		},
		"cells": map[string]any{
			"values": []any{tContainers, tCPUAvg, tCPUMax, tMemAvg, tMemMax, tMemPctMax},
			"fill":   map[string]any{"color": "#1e1e1e"},
			"font":   map[string]any{"color": "#ddd", "size": 10},
			"align":  "left",
		},
		"domain": map[string]any{
			"x": []float64{0.78, 1.0},
			"y": []float64{0.0, 0.2},
		},
	})

	// Layout mimicking make_subplots(3 rows, 2 cols) with plotly_dark.
	layout := map[string]any{
		"template":   "plotly_dark",
		"title":      map[string]any{"text": "Container Resource Monitor", "font": map[string]any{"size": 20}},
		"height":     950,
		"width":      1400,
		"uirevision": "live-monitor",
		"legend": map[string]any{
			"orientation": "h",
			"yanchor":     "bottom",
			"y":           1.02,
			"xanchor":     "center",
			"x":           0.35,
			"font":        map[string]any{"size": 10},
		},
		"barmode":   "group",
		"hovermode": "x unified",

		// Row 1 left - CPU time series
		"xaxis": map[string]any{
			"domain": []float64{0.0, 0.62},
			"anchor": "y",
		},
		"yaxis": map[string]any{
			"domain": []float64{0.72, 1.0},
			"anchor": "x",
			"title":  map[string]any{"text": "CPU %"},
		},

		// Row 1 right - CPU bars
		"xaxis2": map[string]any{
			"domain":    []float64{0.78, 1.0},
			"anchor":    "y2",
			"tickangle": -35,
		},
		"yaxis2": map[string]any{
			"domain": []float64{0.72, 1.0},
			"anchor": "x2",
		},

		// Row 2 left - RAM time series
		"xaxis3": map[string]any{
			"domain": []float64{0.0, 0.62},
			"anchor": "y3",
		},
		"yaxis3": map[string]any{
			"domain": []float64{0.36, 0.64},
			"anchor": "x3",
			"title":  map[string]any{"text": "MB"},
		},

		// Row 2 right - RAM bars
		"xaxis4": map[string]any{
			"domain":    []float64{0.78, 1.0},
			"anchor":    "y4",
			"tickangle": -35,
		},
		"yaxis4": map[string]any{
			"domain": []float64{0.36, 0.64},
			"anchor": "x4",
		},

		// Row 3 left - Mem % time series
		"xaxis5": map[string]any{
			"domain": []float64{0.0, 0.62},
			"anchor": "y5",
			"title":  map[string]any{"text": "Time"},
			"rangeslider": map[string]any{
				"visible":   true,
				"thickness": 0.05,
			},
		},
		"yaxis5": map[string]any{
			"domain": []float64{0.0, 0.2},
			"anchor": "x5",
			"title":  map[string]any{"text": "Mem %"},
		},

		// Subplot titles as annotations.
		"annotations": []map[string]any{
			subplotTitle("CPU %", 0.31, 1.0),
			subplotTitle("CPU - peak & average", 0.89, 1.0),
			subplotTitle("RAM (MB)", 0.31, 0.64),
			subplotTitle("RAM - peak & average", 0.89, 0.64),
			subplotTitle("Memory % of limit", 0.31, 0.2),
		},
	}

	return map[string]any{
		"data":   traces,
		"layout": layout,
	}
}

func subplotTitle(text string, x, y float64) map[string]any {
	return map[string]any{
		"text":      fmt.Sprintf("<b>%s</b>", text),
		"x":         x,
		"y":         y,
		"xref":      "paper",
		"yref":      "paper",
		"xanchor":   "center",
		"yanchor":   "bottom",
		"showarrow": false,
		"font":      map[string]any{"size": 14},
	}
}

func emptyFigure() map[string]any {
	return map[string]any{
		"data": []any{},
		"layout": map[string]any{
			"template": "plotly_dark",
			"title":    map[string]any{"text": "Container Resource Monitor", "font": map[string]any{"size": 20}},
			"height":   600,
			"width":    1200,
			"annotations": []map[string]any{
				{
					"x":         0.5,
					"y":         0.5,
					"xref":      "paper",
					"yref":      "paper",
					"showarrow": false,
					"font":      map[string]any{"size": 18},
					"text":      "No metrics yet. Start d-daemon.sh or k8s-daemon.sh and wait for samples.",
				},
			},
		},
	}
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

var termColors = []ui.Color{
	ui.ColorBlue,
	ui.ColorRed,
	ui.Color(42),  // green
	ui.ColorMagenta,
	ui.Color(208), // orange
	ui.ColorCyan,
	ui.Color(204), // pink
	ui.Color(149), // light green
	ui.Color(213), // magenta-pink
	ui.Color(220), // yellow
}

func truncName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func runTerm(args []string) {
	fs := flag.NewFlagSet("term", flag.ExitOnError)
	csvPath := fs.String("csv", "docker-stats.csv", "Path to CSV file")
	interval := fs.Float64("interval", 2.0, "Refresh interval in seconds")
	fs.Parse(args)
	if fs.NArg() > 0 {
		*csvPath = fs.Arg(0)
	}

	if err := ui.Init(); err != nil {
		log.Fatalf("failed to init termui: %v", err)
	}
	defer ui.Close()

	cpuPlot := widgets.NewPlot()
	cpuPlot.Title = " CPU % "
	cpuPlot.AxesColor = ui.ColorWhite
	cpuPlot.ShowAxes = true

	ramPlot := widgets.NewPlot()
	ramPlot.Title = " RAM (MB) "
	ramPlot.AxesColor = ui.ColorWhite
	ramPlot.ShowAxes = true

	cpuBar := widgets.NewBarChart()
	cpuBar.Title = " CPU peak % "
	cpuBar.BarWidth = 5
	cpuBar.BarGap = 1

	ramBar := widgets.NewBarChart()
	ramBar.Title = " RAM peak MB "
	ramBar.BarWidth = 5
	ramBar.BarGap = 1

	table := widgets.NewTable()
	table.Title = " Summary "
	table.TextStyle = ui.NewStyle(ui.ColorWhite)
	table.RowSeparator = true
	table.TextAlignment = ui.AlignCenter

	statusBar := widgets.NewParagraph()
	statusBar.Border = false
	statusBar.TextStyle = ui.NewStyle(ui.ColorWhite)

	grid := ui.NewGrid()
	termWidth, termHeight := ui.TerminalDimensions()
	grid.SetRect(0, 0, termWidth, termHeight-1)
	grid.Set(
		ui.NewRow(0.37,
			ui.NewCol(0.7, cpuPlot),
			ui.NewCol(0.3, cpuBar),
		),
		ui.NewRow(0.37,
			ui.NewCol(0.7, ramPlot),
			ui.NewCol(0.3, ramBar),
		),
		ui.NewRow(0.26, table),
	)
	statusBar.SetRect(0, termHeight-1, termWidth, termHeight)

	updateData := func() {
		records, err := loadCSV(*csvPath)
		if err != nil || len(records) == 0 {
			table.Rows = [][]string{{"Waiting for data..."}, {fmt.Sprintf("CSV: %s", *csvPath)}}
			statusBar.Text = fmt.Sprintf(" [%s](fg:cyan) | q to quit | no data yet",
				time.Now().Format("15:04:05"))
			ui.Render(grid, statusBar)
			return
		}

		seen := map[string]bool{}
		for _, r := range records {
			seen[r.Container] = true
		}
		containers := make([]string, 0, len(seen))
		for c := range seen {
			containers = append(containers, c)
		}
		sort.Strings(containers)

		tsSet := map[time.Time]bool{}
		for _, r := range records {
			tsSet[r.Timestamp] = true
		}
		timestamps := make([]time.Time, 0, len(tsSet))
		for ts := range tsSet {
			timestamps = append(timestamps, ts)
		}
		sort.Slice(timestamps, func(i, j int) bool {
			return timestamps[i].Before(timestamps[j])
		})

		lookup := map[string]map[time.Time]record{}
		for _, r := range records {
			if _, ok := lookup[r.Container]; !ok {
				lookup[r.Container] = map[time.Time]record{}
			}
			lookup[r.Container][r.Timestamp] = r
		}

		cpuData := make([][]float64, len(containers))
		ramData := make([][]float64, len(containers))
		plotLabels := make([]string, len(containers))
		plotColors := make([]ui.Color, len(containers))

		for i, c := range containers {
			cpuSeries := make([]float64, len(timestamps))
			ramSeries := make([]float64, len(timestamps))
			for j, ts := range timestamps {
				if r, ok := lookup[c][ts]; ok {
					cpuSeries[j] = r.CPUPct
					ramSeries[j] = r.MemUsageMB
				}
			}
			cpuData[i] = cpuSeries
			ramData[i] = ramSeries
			plotLabels[i] = c
			plotColors[i] = termColors[i%len(termColors)]
		}

		cpuPlot.Data = cpuData
		cpuPlot.DataLabels = plotLabels
		cpuPlot.LineColors = plotColors

		ramPlot.Data = ramData
		ramPlot.DataLabels = plotLabels
		ramPlot.LineColors = plotColors

		stats := map[string]*containerStats{}
		for _, r := range records {
			s, ok := stats[r.Container]
			if !ok {
				s = &containerStats{}
				stats[r.Container] = s
			}
			s.CPUSum += r.CPUPct
			if r.CPUPct > s.CPUMax {
				s.CPUMax = r.CPUPct
			}
			s.MemSum += r.MemUsageMB
			if r.MemUsageMB > s.MemMax {
				s.MemMax = r.MemUsageMB
			}
			if r.MemPct > s.MemPctMax {
				s.MemPctMax = r.MemPct
			}
			s.Count++
		}

		cpuPeakVals := make([]float64, len(containers))
		ramPeakVals := make([]float64, len(containers))
		barLabels := make([]string, len(containers))
		barColors := make([]ui.Color, len(containers))
		for i, c := range containers {
			s := stats[c]
			cpuPeakVals[i] = round1(s.CPUMax)
			ramPeakVals[i] = round1(s.MemMax)
			barLabels[i] = truncName(c, 6)
			barColors[i] = termColors[i%len(termColors)]
		}
		cpuBar.Data = cpuPeakVals
		cpuBar.Labels = barLabels
		cpuBar.BarColors = barColors
		ramBar.Data = ramPeakVals
		ramBar.Labels = barLabels
		ramBar.BarColors = barColors

		rows := [][]string{
			{"Container", "CPU avg%", "CPU max%", "RAM avg MB", "RAM max MB", "Mem max%"},
		}
		for _, c := range containers {
			s := stats[c]
			rows = append(rows, []string{
				c,
				fmt.Sprintf("%.1f", s.CPUSum/float64(s.Count)),
				fmt.Sprintf("%.1f", s.CPUMax),
				fmt.Sprintf("%.1f", s.MemSum/float64(s.Count)),
				fmt.Sprintf("%.1f", s.MemMax),
				fmt.Sprintf("%.2f", s.MemPctMax),
			})
		}
		table.Rows = rows
		table.RowStyles = map[int]ui.Style{
			0: ui.NewStyle(ui.ColorYellow, ui.ColorClear, ui.ModifierBold),
		}

		last := timestamps[len(timestamps)-1].Format("15:04:05")
		statusBar.Text = fmt.Sprintf(
			" [%s](fg:cyan) | CSV: [%s](fg:green) | %d containers | %d samples | last: %s | q to quit",
			time.Now().Format("15:04:05"), *csvPath, len(containers), len(timestamps), last,
		)

		ui.Render(grid, statusBar)
	}

	updateData()

	ticker := time.NewTicker(time.Duration(float64(time.Second) * *interval))
	defer ticker.Stop()

	uiEvents := ui.PollEvents()
	for {
		select {
		case e := <-uiEvents:
			switch e.ID {
			case "q", "<C-c>":
				return
			case "<Resize>":
				payload := e.Payload.(ui.Resize)
				grid.SetRect(0, 0, payload.Width, payload.Height-1)
				statusBar.SetRect(0, payload.Height-1, payload.Width, payload.Height)
				ui.Clear()
				updateData()
			}
		case <-ticker.C:
			updateData()
		}
	}
}

func liveHTML(interval float64, csvPath string) string {
	refreshMs := int(interval * 1000)
	if refreshMs < 500 {
		refreshMs = 500
	}
	escaped := html.EscapeString(csvPath)
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Container Monitor Live</title>
  <script src="https://cdn.plot.ly/plotly-2.35.2.min.js"></script>
  <style>
    body {
      margin: 0;
      padding: 12px;
      background: #11161d;
      color: #dce3f0;
      font: 13px/1.4 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    .meta {
      margin-bottom: 8px;
      opacity: 0.9;
    }
    #chart {
      width: 100%%;
      height: calc(100vh - 56px);
      min-height: 560px;
      border-radius: 8px;
      overflow: hidden;
      background: #0f141b;
      border: 1px solid rgba(120, 140, 170, 0.25);
    }
    code {
      color: #8ed7ff;
    }
  </style>
</head>
<body>
  <div class="meta">
    Source: <code>%s</code>
    | Refresh: <code>%.1fs</code>
    | Last update: <span id="updated">-</span>
  </div>
  <div id="chart"></div>
  <script>
    const REFRESH_MS = %d;
    const chart = document.getElementById("chart");
    const updated = document.getElementById("updated");

    async function updateFigure() {
      try {
        const response = await fetch("/api/figure?ts=" + Date.now(), { cache: "no-store" });
        if (!response.ok) {
          throw new Error("HTTP " + response.status);
        }
        const figure = await response.json();
        Plotly.react(chart, figure.data, figure.layout, {
          responsive: true,
          displaylogo: false,
          scrollZoom: true
        });
        updated.textContent = new Date().toLocaleTimeString();
      } catch (error) {
        updated.textContent = "update failed: " + error.message;
      }
    }

    updateFigure();
    setInterval(updateFigure, REFRESH_MS);
    window.addEventListener("resize", () => Plotly.Plots.resize(chart));
  </script>
</body>
</html>`, escaped, interval, refreshMs)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Start()
}

func runPlot(args []string) {
	fs := flag.NewFlagSet("plot", flag.ExitOnError)
	csvPath := fs.String("csv", "docker-stats.csv", "Path to CSV file")
	live := fs.Bool("live", false, "Serve live-updating dashboard")
	interval := fs.Float64("interval", 2.0, "Refresh interval in seconds for live mode")
	host := fs.String("host", "127.0.0.1", "Host for live server")
	port := fs.Int("port", 8088, "Port for live server")
	noOpen := fs.Bool("no-open-browser", false, "Do not auto-open browser")
	fs.Parse(args)

	if fs.NArg() > 0 {
		*csvPath = fs.Arg(0)
	}

	if !*live {
		records, err := loadCSV(*csvPath)
		if err != nil {
			log.Fatalf("Error reading CSV: %v", err)
		}
		fig := buildFigure(records)
		figJSON, _ := json.Marshal(fig)

		outPath := strings.TrimSuffix(*csvPath, ".csv") + ".html"
		outHTML := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>Container Resource Monitor</title>
  <script src="https://cdn.plot.ly/plotly-2.35.2.min.js"></script>
  <style>body{margin:0;background:#11161d}</style>
</head>
<body>
  <div id="chart"></div>
  <script>
    const figure = %s;
    Plotly.newPlot("chart", figure.data, figure.layout, {responsive:true,displaylogo:false,scrollZoom:true});
  </script>
</body>
</html>`, string(figJSON))

		if err := os.WriteFile(outPath, []byte(outHTML), 0644); err != nil {
			log.Fatalf("Error writing HTML: %v", err)
		}
		fmt.Printf("Saved interactive dashboard -> %s\n", outPath)
		openBrowser(outPath)
		return
	}

	if *interval <= 0 {
		log.Fatal("--interval must be > 0")
	}

	addr := fmt.Sprintf("%s:%d", *host, *port)
	fmt.Printf("Live mode: http://%s\n", addr)
	fmt.Printf("Source CSV: %s\n", *csvPath)
	fmt.Printf("Refresh interval: %.1fs\n", *interval)
	fmt.Println("Press Ctrl+C to stop")

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/" && p != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, liveHTML(*interval, *csvPath))
	})

	mux.HandleFunc("/api/figure", func(w http.ResponseWriter, r *http.Request) {
		records, err := loadCSV(*csvPath)
		if err != nil {
			records = nil
		}
		fig := buildFigure(records)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(fig)
	})

	if !*noOpen {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(fmt.Sprintf("http://%s", addr))
		}()
	}

	log.Fatal(http.ListenAndServe(addr, mux))
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: cstats <command> [flags]

Commands:
  plot    HTML/Plotly dashboard (one-shot or live server)
  term    Terminal UI dashboard

Run "cstats <command> -h" for command-specific flags.
`)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "plot":
		runPlot(os.Args[2:])
	case "term":
		runTerm(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		usage()
	}
}
