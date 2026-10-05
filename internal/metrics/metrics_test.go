package metrics

import (
	"strings"
	"testing"
)

func TestWriteText(t *testing.T) {
	r := New()
	c := r.Counter("x_total", "Things.", "kind")
	c.Inc("a")
	c.Add(2, `b"q`)
	r.Counter("y_total", "Plain.").Inc()
	r.GaugeFunc("z", "A gauge.", func() float64 { return 7 })
	var b strings.Builder
	r.WriteText(&b)
	want := `# HELP x_total Things.
# TYPE x_total counter
x_total{kind="a"} 1
x_total{kind="b\"q"} 2
# HELP y_total Plain.
# TYPE y_total counter
y_total 1
# HELP z A gauge.
# TYPE z gauge
z 7
`
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
	if r.Counter("x_total", "") != c {
		t.Fatal("Counter did not return the existing counter")
	}
}
