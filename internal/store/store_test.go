package store

import "testing"

func TestRingWrapAndOrder(t *testing.T) {
	s := New(3)
	for i := 1; i <= 5; i++ {
		s.Add("a", nil, Point{T: int64(i), V: float64(i)})
	}
	d := s.Data("a")
	if len(d) != 3 || d[0].T != 3 || d[2].T != 5 {
		t.Fatalf("got %+v", d)
	}
	// duplicate/old timestamps ignored
	s.Add("a", nil, Point{T: 5, V: 99})
	if d := s.Data("a"); d[2].V != 5 {
		t.Fatalf("duplicate not ignored: %+v", d)
	}
}

func TestSubscribe(t *testing.T) {
	s := New(10)
	ch, cancel := s.Subscribe()
	defer cancel()
	s.Add("x", map[string]string{"k": "v"}, Point{T: 1, V: 2})
	u := <-ch
	if u.ID != "x" || u.Point.V != 2 {
		t.Fatalf("got %+v", u)
	}
}
