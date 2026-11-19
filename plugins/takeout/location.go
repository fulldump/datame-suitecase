package takeout

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Location history comes, in the exports that still have it, as Records.json:
// every point the phone sent, for years, sometimes gigabytes. It is copied as
// it comes and summed up in one CSV per year (date, latitude, longitude,
// accuracy), which any spreadsheet or map program opens. It is read as a
// stream, point by point, and each year is written to a temporary file.

type locationPoint struct {
	LatE7       *int64 `json:"latitudeE7"`
	LonE7       *int64 `json:"longitudeE7"`
	Timestamp   string `json:"timestamp"`
	TimestampMs string `json:"timestampMs"`
	Accuracy    *int64 `json:"accuracy"`
}

func (p locationPoint) when() time.Time {
	if t, err := time.Parse(time.RFC3339Nano, p.Timestamp); err == nil {
		return t.UTC()
	}
	if ms, err := strconv.ParseInt(p.TimestampMs, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC()
	}
	return time.Time{}
}

type yearCSV struct {
	file   *os.File
	buffer *bufio.Writer
	writer *csv.Writer
	last   time.Time
}

func (x *importer) records(ctx context.Context, e *entry, out string) error {
	if _, err := x.raw(ctx, e, out, ""); err != nil {
		return err
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	defer r.Close()
	years := map[int]*yearCSV{}
	defer func() {
		for _, y := range years {
			y.file.Close()
			os.Remove(y.file.Name())
		}
	}()
	add := func(p locationPoint) error {
		when := p.when()
		if when.IsZero() || p.LatE7 == nil || p.LonE7 == nil {
			return nil
		}
		y := years[when.Year()]
		if y == nil {
			f, err := os.CreateTemp(x.tmp, "ubicaciones-*.csv")
			if err != nil {
				return err
			}
			buffer := bufio.NewWriterSize(f, 256<<10)
			y = &yearCSV{file: f, buffer: buffer, writer: csv.NewWriter(buffer)}
			years[when.Year()] = y
			y.writer.Write([]string{"fecha_utc", "latitud", "longitud", "precision_m"})
		}
		accuracy := ""
		if p.Accuracy != nil {
			accuracy = strconv.FormatInt(*p.Accuracy, 10)
		}
		if when.After(y.last) {
			y.last = when
		}
		return y.writer.Write([]string{when.Format(time.RFC3339), strconv.FormatFloat(float64(*p.LatE7)/1e7, 'f', 7, 64),
			strconv.FormatFloat(float64(*p.LonE7)/1e7, 'f', 7, 64), accuracy})
	}
	if err := readLocations(ctx, r, add); err != nil {
		x.run.Logf("%s no se pudo resumir: %s (se ha guardado tal cual)", e.name, err)
		return nil
	}

	list := make([]int, 0, len(years))
	for year := range years {
		list = append(list, year)
	}
	sort.Ints(list)
	for _, year := range list {
		y := years[year]
		y.writer.Flush()
		if err := y.writer.Error(); err != nil {
			return err
		}
		if err := y.buffer.Flush(); err != nil {
			return err
		}
		info, err := y.file.Stat()
		if err != nil {
			return err
		}
		name := y.file.Name()
		file := plugin.File{Path: path.Dir(out) + "/resumen/" + strconv.Itoa(year) + ".csv", ModTime: y.last, Size: info.Size(), Mutable: true,
			Open: func() (io.ReadCloser, error) { return os.Open(filepath.Clean(name)) }}
		if _, err := x.run.Out.Write(ctx, file); err != nil {
			return err
		}
	}
	return nil
}

// readLocations walks {"locations": [ {...}, … ]} one point at a time.
func readLocations(ctx context.Context, r io.Reader, fn func(locationPoint) error) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return fmt.Errorf("no empieza por un objeto")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if key, _ := t.(string); key != "locations" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('[') {
			return fmt.Errorf("«locations» no es una lista")
		}
		for n := 0; dec.More(); n++ {
			if n%10000 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			var p locationPoint
			if err := dec.Decode(&p); err != nil {
				return err
			}
			if err := fn(p); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	return nil
}
