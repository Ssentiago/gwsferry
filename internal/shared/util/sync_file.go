package util

import "os"

// SyncFile — обёртка над *os.File, вызывает Sync() после каждой записи.
// Нужна для lnav/tail -f, чтобы записи появлялись сразу.
type SyncFile struct{ F *os.File }

func (s *SyncFile) Write(p []byte) (int, error) {
	n, err := s.F.Write(p)
	if err != nil {
		return n, err
	}
	s.F.Sync()
	return n, nil
}
