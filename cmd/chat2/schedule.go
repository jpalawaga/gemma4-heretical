package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultScheduleUpcomingWindow = 24 * time.Hour
	defaultScheduleHistoryWindow  = 24 * time.Hour
	maxScheduleDueItems           = 5
	maxScheduleUpcomingItems      = 5
	maxScheduleCompletedItems     = 5
	maxRecurrencePeriod           = 10_000
)

type scheduleEntry struct {
	ID               int        `json:"id"`
	Time             time.Time  `json:"time"`
	Event            string     `json:"event"`
	CreatedAt        *time.Time `json:"created_at,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	SeriesID         int        `json:"series_id,omitempty"`
	RecurrenceUnit   string     `json:"recurrence_unit,omitempty"`
	RecurrencePeriod int        `json:"recurrence_period,omitempty"`
}

type scheduleSnapshot struct {
	Due             []scheduleEntry
	Upcoming        []scheduleEntry
	Later           []scheduleEntry
	RecentCompleted []scheduleEntry
}

type scheduleStore struct {
	path     string
	lockPath string
	mu       sync.Mutex
}

func newScheduleStore(dir string) *scheduleStore {
	path := filepath.Join(dir, "schedule.jsonl")
	return &scheduleStore{path: path, lockPath: path + ".lock"}
}

func (s *scheduleStore) Add(when time.Time, event string, now time.Time) (scheduleEntry, error) {
	event = strings.TrimSpace(event)
	if when.IsZero() {
		return scheduleEntry{}, errors.New("scheduled time is required")
	}
	if event == "" {
		return scheduleEntry{}, errors.New("scheduled event is required")
	}
	var added scheduleEntry
	err := s.withLock(func() error {
		entries, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		nextID := 1
		for _, entry := range entries {
			if entry.ID >= nextID {
				nextID = entry.ID + 1
			}
		}
		createdAt := now
		added = scheduleEntry{
			ID:        nextID,
			Time:      when,
			Event:     event,
			CreatedAt: &createdAt,
		}
		entries = append(entries, added)
		return s.writeUnlocked(entries)
	})
	return added, err
}

func (s *scheduleStore) AddRecurring(when time.Time, event, unit string, period int, now time.Time) (scheduleEntry, error) {
	event = strings.TrimSpace(event)
	unit = strings.ToLower(strings.TrimSpace(unit))
	if when.IsZero() {
		return scheduleEntry{}, errors.New("scheduled time is required")
	}
	if event == "" {
		return scheduleEntry{}, errors.New("scheduled event is required")
	}
	if err := validateRecurrence(unit, period); err != nil {
		return scheduleEntry{}, err
	}
	var added scheduleEntry
	err := s.withLock(func() error {
		entries, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		nextID := nextScheduleID(entries)
		createdAt := now
		added = scheduleEntry{
			ID:               nextID,
			Time:             when,
			Event:            event,
			CreatedAt:        &createdAt,
			SeriesID:         nextID,
			RecurrenceUnit:   unit,
			RecurrencePeriod: period,
		}
		entries = append(entries, added)
		entries, err = materializeRecurringEntries(entries, now, defaultScheduleUpcomingWindow)
		if err != nil {
			return err
		}
		return s.writeUnlocked(entries)
	})
	return added, err
}

func (s *scheduleStore) Complete(id int, now time.Time) (scheduleEntry, error) {
	if id <= 0 {
		return scheduleEntry{}, errors.New("schedule ID must be greater than zero")
	}
	var completed scheduleEntry
	err := s.withLock(func() error {
		entries, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		found := false
		for index := range entries {
			if entries[index].ID != id {
				continue
			}
			if entries[index].CompletedAt != nil {
				return fmt.Errorf("scheduled item #%d is already complete", id)
			}
			finishedAt := now
			entries[index].CompletedAt = &finishedAt
			completed = entries[index]
			found = true
			break
		}
		if !found {
			return fmt.Errorf("no scheduled item #%d", id)
		}
		return s.writeUnlocked(entries)
	})
	return completed, err
}

func (s *scheduleStore) List() ([]scheduleEntry, error) {
	var entries []scheduleEntry
	err := s.withLock(func() error {
		var err error
		entries, err = s.loadUnlocked()
		return err
	})
	sortScheduleEntries(entries)
	return entries, err
}

func (s *scheduleStore) Snapshot(now time.Time, upcomingWindow, historyWindow time.Duration) (scheduleSnapshot, error) {
	if err := s.MaterializeRecurrences(now, upcomingWindow); err != nil {
		return scheduleSnapshot{}, err
	}
	entries, err := s.List()
	if err != nil {
		return scheduleSnapshot{}, err
	}
	var snapshot scheduleSnapshot
	upcomingEnd := now.Add(upcomingWindow)
	historyStart := now.Add(-historyWindow)
	for _, entry := range entries {
		if entry.CompletedAt != nil {
			if historyWindow > 0 && !entry.CompletedAt.Before(historyStart) {
				snapshot.RecentCompleted = append(snapshot.RecentCompleted, entry)
			}
			continue
		}
		if !entry.Time.After(now) {
			snapshot.Due = append(snapshot.Due, entry)
			continue
		}
		if upcomingWindow > 0 && !entry.Time.After(upcomingEnd) {
			snapshot.Upcoming = append(snapshot.Upcoming, entry)
			continue
		}
		if len(snapshot.Later) == 0 {
			snapshot.Later = append(snapshot.Later, entry)
		}
	}
	snapshot.Due = lastScheduleEntries(snapshot.Due, maxScheduleDueItems)
	snapshot.Upcoming = firstScheduleEntries(snapshot.Upcoming, maxScheduleUpcomingItems)
	snapshot.RecentCompleted = lastScheduleEntries(snapshot.RecentCompleted, maxScheduleCompletedItems)
	return snapshot, nil
}

func (s *scheduleStore) MaterializeRecurrences(now time.Time, window time.Duration) error {
	return s.withLock(func() error {
		entries, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		expanded, err := materializeRecurringEntries(entries, now, window)
		if err != nil {
			return err
		}
		if len(expanded) == len(entries) {
			return nil
		}
		return s.writeUnlocked(expanded)
	})
}

func materializeRecurringEntries(entries []scheduleEntry, now time.Time, window time.Duration) ([]scheduleEntry, error) {
	latestBySeries := make(map[int]scheduleEntry)
	seriesIDs := make([]int, 0)
	nextID := nextScheduleID(entries)
	for _, entry := range entries {
		if entry.SeriesID == 0 {
			continue
		}
		if err := validateRecurrence(entry.RecurrenceUnit, entry.RecurrencePeriod); err != nil {
			return nil, fmt.Errorf("recurring series #%d: %w", entry.SeriesID, err)
		}
		latest, exists := latestBySeries[entry.SeriesID]
		if !exists {
			seriesIDs = append(seriesIDs, entry.SeriesID)
		}
		if !exists || entry.Time.After(latest.Time) || (entry.Time.Equal(latest.Time) && entry.ID > latest.ID) {
			latestBySeries[entry.SeriesID] = entry
		}
	}
	sort.Ints(seriesIDs)
	horizon := now.Add(window)
	for _, seriesID := range seriesIDs {
		latest := latestBySeries[seriesID]
		if latest.Time.After(horizon) {
			continue
		}
		next := nextRecurrenceTime(latest.Time, latest.RecurrenceUnit, latest.RecurrencePeriod)
		for {
			createdAt := now
			entry := scheduleEntry{
				ID:               nextID,
				Time:             next,
				Event:            latest.Event,
				CreatedAt:        &createdAt,
				SeriesID:         seriesID,
				RecurrenceUnit:   latest.RecurrenceUnit,
				RecurrencePeriod: latest.RecurrencePeriod,
			}
			entries = append(entries, entry)
			nextID++
			if next.After(horizon) {
				break
			}
			next = nextRecurrenceTime(next, latest.RecurrenceUnit, latest.RecurrencePeriod)
		}
	}
	sortScheduleEntries(entries)
	return entries, nil
}

func validateRecurrence(unit string, period int) error {
	if period <= 0 {
		return errors.New("recurrence_period must be greater than zero")
	}
	if period > maxRecurrencePeriod {
		return fmt.Errorf("recurrence_period must not exceed %d", maxRecurrencePeriod)
	}
	switch unit {
	case "hour", "day", "week", "month":
		return nil
	default:
		return fmt.Errorf("recurrence_unit must be hour, day, week, or month, got %q", unit)
	}
}

func nextRecurrenceTime(value time.Time, unit string, period int) time.Time {
	switch unit {
	case "hour":
		return value.Add(time.Duration(period) * time.Hour)
	case "day":
		return value.AddDate(0, 0, period)
	case "week":
		return value.AddDate(0, 0, 7*period)
	case "month":
		return value.AddDate(0, period, 0)
	default:
		return value
	}
}

func nextScheduleID(entries []scheduleEntry) int {
	nextID := 1
	for _, entry := range entries {
		if entry.ID >= nextID {
			nextID = entry.ID + 1
		}
	}
	return nextID
}

func firstScheduleEntries(entries []scheduleEntry, limit int) []scheduleEntry {
	if len(entries) <= limit {
		return entries
	}
	return entries[:limit]
}

func lastScheduleEntries(entries []scheduleEntry, limit int) []scheduleEntry {
	if len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
}

func (s *scheduleStore) withLock(action func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return action()
}

func (s *scheduleStore) loadUnlocked() ([]scheduleEntry, error) {
	file, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []scheduleEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var entry scheduleEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("decode schedule line %d: %w", line, err)
		}
		if entry.ID <= 0 || entry.Time.IsZero() || strings.TrimSpace(entry.Event) == "" {
			return nil, fmt.Errorf("schedule line %d is missing id, time, or event", line)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *scheduleStore) writeUnlocked(entries []scheduleEntry) error {
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".schedule-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	encoder := json.NewEncoder(temporary)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			temporary.Close()
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func sortScheduleEntries(entries []scheduleEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Time.Equal(entries[j].Time) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Time.Before(entries[j].Time)
	})
}

func parseScheduleTime(value string, location *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("scheduled time is required")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized scheduled time %q; use RFC3339 or YYYY-MM-DD HH:MM", value)
}

type scheduleItemView struct {
	ID               int    `json:"id"`
	Time             string `json:"time"`
	Event            string `json:"event"`
	Status           string `json:"status"`
	CompletedAt      string `json:"completed_at,omitempty"`
	SeriesID         int    `json:"series_id,omitempty"`
	RecurrenceUnit   string `json:"recurrence_unit,omitempty"`
	RecurrencePeriod int    `json:"recurrence_period,omitempty"`
}

func scheduleView(entry scheduleEntry, now time.Time) scheduleItemView {
	status := "upcoming"
	completedAt := ""
	if entry.CompletedAt != nil {
		status = "completed"
		completedAt = entry.CompletedAt.Format(time.RFC3339)
	} else if !entry.Time.After(now) {
		status = "due"
	}
	return scheduleItemView{
		ID:               entry.ID,
		Time:             entry.Time.Format(time.RFC3339),
		Event:            entry.Event,
		Status:           status,
		CompletedAt:      completedAt,
		SeriesID:         entry.SeriesID,
		RecurrenceUnit:   entry.RecurrenceUnit,
		RecurrencePeriod: entry.RecurrencePeriod,
	}
}

func scheduleViews(entries []scheduleEntry, now time.Time) []scheduleItemView {
	result := make([]scheduleItemView, 0, len(entries))
	for _, entry := range entries {
		result = append(result, scheduleView(entry, now))
	}
	return result
}
