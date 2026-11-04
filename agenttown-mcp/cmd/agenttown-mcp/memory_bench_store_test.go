package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/storage"
)

// benchStore implements the production Store contract without a database. Each
// run owns a separate instance; rebuilding an agent retains only this instance.
type benchStore struct {
	mu          sync.Mutex
	schedules   map[string]storage.ScheduleState
	episodes    map[string]storage.Episode
	commitments map[string]storage.Commitment
	people      map[string]storage.Person
	actions     map[string][]storage.ActionRecord
}

func newBenchStore() *benchStore {
	return &benchStore{
		schedules: map[string]storage.ScheduleState{}, episodes: map[string]storage.Episode{},
		commitments: map[string]storage.Commitment{}, people: map[string]storage.Person{}, actions: map[string][]storage.ActionRecord{},
	}
}

func benchCopy[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *benchStore) LoadScheduleState(ctx context.Context, id string) (storage.ScheduleState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.schedules[id]
	if !ok {
		return v, storage.ErrNotFound
	}
	return v, ctx.Err()
}
func (s *benchStore) SaveScheduleState(ctx context.Context, id string, v storage.ScheduleState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules[id] = v
	return ctx.Err()
}
func (s *benchStore) SaveEpisode(ctx context.Context, v storage.Episode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.episodes[v.ID]; ok {
		return fmt.Errorf("duplicate episode")
	}
	s.episodes[v.ID] = benchCopy(v)
	return ctx.Err()
}
func (s *benchStore) LoadEpisodes(ctx context.Context, id string, q storage.EpisodeQuery) ([]storage.Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []storage.Episode{}
	for _, e := range s.episodes {
		if e.OwnerID != id || q.Category != "" && e.Category != q.Category || q.GameTimeMin > 0 && e.GameTime < q.GameTimeMin || q.GameTimeMax > 0 && e.GameTime > q.GameTimeMax {
			continue
		}
		match := true
		for _, k := range q.Keywords {
			if !strings.Contains(strings.ToLower(e.Summary), strings.ToLower(k)) {
				match = false
			}
		}
		if match {
			out = append(out, benchCopy(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GameTime == out[j].GameTime {
			return out[i].ID < out[j].ID
		}
		return out[i].GameTime > out[j].GameTime
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, ctx.Err()
}
func (s *benchStore) LoadLatestDailySummary(ctx context.Context, id string) (storage.Episode, error) {
	v, e := s.LoadEpisodes(ctx, id, storage.EpisodeQuery{Category: "daily_summary", Limit: 1})
	if e != nil {
		return storage.Episode{}, e
	}
	if len(v) == 0 {
		return storage.Episode{}, storage.ErrNotFound
	}
	return v[0], nil
}
func (s *benchStore) SaveCommitment(ctx context.Context, v storage.Commitment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.commitments[v.ID]; ok {
		return fmt.Errorf("duplicate commitment")
	}
	s.commitments[v.ID] = v
	return ctx.Err()
}
func (s *benchStore) UpdateCommitment(ctx context.Context, id, status string, start, end int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.commitments[id]
	if !ok {
		return nil
	}
	if status != "" {
		v.Status = status
	}
	if start >= 0 {
		v.DueStart = start
	}
	if end >= 0 {
		v.DueEnd = end
	}
	v.UpdatedTime = time.Now().Unix()
	s.commitments[id] = v
	return ctx.Err()
}
func (s *benchStore) LoadCommitments(ctx context.Context, id string, q storage.CommitmentQuery) ([]storage.Commitment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []storage.Commitment{}
	for _, v := range s.commitments {
		if v.OwnerID != id || q.Status != "" && v.Status != q.Status || q.CounterpartID != "" && v.CounterpartID != q.CounterpartID || q.DueStartMax > 0 && v.DueStart > q.DueStartMax || q.DueEndMin > 0 && v.DueEnd < q.DueEndMin {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DueStart == out[j].DueStart {
			return out[i].ID < out[j].ID
		}
		return out[i].DueStart < out[j].DueStart
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, ctx.Err()
}
func (s *benchStore) UpsertPerson(ctx context.Context, p storage.Person, delta int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := p.OwnerID + "/" + p.TargetID
	old, ok := s.people[key]
	if !ok {
		old.Affinity = 50
	}
	p.Affinity = old.Affinity + delta
	if p.Affinity < 0 {
		p.Affinity = 0
	}
	if p.Affinity > 100 {
		p.Affinity = 100
	}
	s.people[key] = p
	return ctx.Err()
}
func (s *benchStore) SeedPerson(ctx context.Context, p storage.Person) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := p.OwnerID + "/" + p.TargetID
	if _, ok := s.people[key]; !ok {
		s.people[key] = p
	}
	return ctx.Err()
}
func (s *benchStore) LoadPeople(ctx context.Context, id string) ([]storage.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []storage.Person{}
	for _, v := range s.people {
		if v.OwnerID == id {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out, ctx.Err()
}
func (s *benchStore) SaveActionRecord(ctx context.Context, id string, r storage.ActionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions[id] = append(s.actions[id], benchCopy(r))
	return ctx.Err()
}
func (s *benchStore) LoadActionHistory(ctx context.Context, id string, n int) ([]storage.ActionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := benchCopy(s.actions[id])
	sort.Slice(v, func(i, j int) bool { return v[i].StartedAt.After(v[j].StartedAt) })
	if n > 0 && len(v) > n {
		v = v[:n]
	}
	return v, ctx.Err()
}
func (s *benchStore) Close() error { return nil }

var _ storage.Store = (*benchStore)(nil)
