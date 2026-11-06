package preview

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type slowResponse struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *slowResponse) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return w.ResponseRecorder.Write(b)
}

func TestSlowReaderDoesNotBlockPublishingOrRemoval(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.html")
	write(t, file, "snapshot")
	s := openStore(t, filepath.Join(root, "state"))
	p, err := s.Publish(context.Background(), file, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := &slowResponse{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	responseDone := make(chan struct{})
	go func() {
		defer close(responseDone)
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/p/"+p.ID+"/a.html", nil))
	}()
	defer func() {
		close(w.release)
		<-responseDone
	}()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("response did not start")
	}
	operations := make(chan error, 1)
	go func() {
		if _, err := s.Publish(context.Background(), file, time.Hour); err != nil {
			operations <- err
			return
		}
		operations <- s.Remove(p.ID)
	}()
	select {
	case err := <-operations:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow response blocked registry mutations")
	}
	if list := s.List(); len(list) != 1 || list[0].ID == p.ID {
		t.Fatalf("unexpected list after concurrent publication and removal: %+v", list)
	}
}
