package dashboard

import (
	"context"
	"testing"

	"terminalmcp/internal/viewer/viewertest"
)

// BenchmarkRecentAtCapacity measures one Recent request on a window holding
// 50,000 occurrences (views are cached; records are read per request).
func BenchmarkRecentAtCapacity(b *testing.B) {
	j := openJournal(b)
	for i := 0; i < 25000; i++ {
		chatStart(j, "r"+itoa(i), "chat-"+itoa(i%50))
		chatFinish(j, "r"+itoa(i))
	}
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, err := mg.Open(context.Background(), s, ModeLive, "")
	if err != nil {
		b.Fatal(err)
	}
	for {
		cov, err := mg.Scan(context.Background(), s, id)
		if err != nil {
			b.Fatal(err)
		}
		if cov.End != "open" {
			break
		}
	}
	m, _ := mg.get(s, id)
	b.Logf("occurrences %d", m.win.proj.Occurrences())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := mg.Recent(context.Background(), s, id, nil); err != nil {
			b.Fatal(err)
		}
	}
}
