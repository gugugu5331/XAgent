package artifact

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"
)

type cleanupCandidate struct {
	id        string
	createdAt time.Time
	bytes     int64
	expired   bool
}

func (s *fileStore) Cleanup(ctx context.Context) (CleanupResult, error) {
	if err := s.available(ctx); err != nil {
		return CleanupResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return CleanupResult{}, errors.New("artifact store is closed")
	}
	select {
	case <-ctx.Done():
		return CleanupResult{}, errors.New("artifact cleanup canceled")
	default:
	}
	if err := s.ensurePrivateRootLocked(); err != nil {
		return CleanupResult{}, errors.New("artifact cleanup unavailable")
	}

	candidates := s.cleanupCandidates()
	selected := selectCleanupCandidates(candidates, s.totalBytes, s.options.MaxTotalBytes)
	result := CleanupResult{}
	for _, candidate := range selected {
		select {
		case <-ctx.Done():
			return result, errors.New("artifact cleanup canceled")
		default:
		}
		err := s.privateRoot.remove(candidate.id + ".artifact")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			result.Failed++
			continue
		}
		delete(s.records, candidate.id)
		s.totalBytes -= candidate.bytes
		if s.totalBytes < 0 {
			s.totalBytes = 0
		}
		result.Removed++
		result.ReclaimedBytes = saturatingAdd(result.ReclaimedBytes, candidate.bytes)
	}
	if result.Failed != 0 {
		return result, errors.New("artifact cleanup incomplete")
	}
	return result, nil
}

func (s *fileStore) cleanupCandidates() []cleanupCandidate {
	cutoff := s.now().UTC().Add(-s.options.Retention)
	candidates := make([]cleanupCandidate, 0, len(s.records))
	for id, record := range s.records {
		if !record.ref.Available || !validArtifactID(id) {
			continue
		}
		bytes := record.ref.Bytes
		if bytes < 0 {
			bytes = 0
		}
		candidates = append(candidates, cleanupCandidate{
			id:        id,
			createdAt: record.ref.CreatedAt.UTC(),
			bytes:     bytes,
			expired:   !record.ref.CreatedAt.UTC().After(cutoff),
		})
	}
	sort.Slice(candidates, func(first, second int) bool {
		if candidates[first].createdAt.Equal(candidates[second].createdAt) {
			return candidates[first].id < candidates[second].id
		}
		return candidates[first].createdAt.Before(candidates[second].createdAt)
	})
	return candidates
}

func selectCleanupCandidates(candidates []cleanupCandidate, totalBytes, maxTotalBytes int64) []cleanupCandidate {
	selected := make([]cleanupCandidate, 0, len(candidates))
	remaining := totalBytes
	retained := make([]cleanupCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.expired {
			selected = append(selected, candidate)
			remaining -= candidate.bytes
			if remaining < 0 {
				remaining = 0
			}
			continue
		}
		retained = append(retained, candidate)
	}
	for _, candidate := range retained {
		if remaining <= maxTotalBytes {
			break
		}
		selected = append(selected, candidate)
		remaining -= candidate.bytes
		if remaining < 0 {
			remaining = 0
		}
	}
	return selected
}
