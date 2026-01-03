package spotify

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teal-fm/piper/db"
	"github.com/teal-fm/piper/models"
	"github.com/teal-fm/piper/session"
)

// ===== Mock Implementations =====

// publishCall records a call to PublishPlayingNow
type publishCall struct {
	userID int64
	track  *models.Track
}

// mockPlayingNowService implements the playingNowService interface for testing
type mockPlayingNowService struct {
	publishCalls []publishCall
	clearCalls   []int64
	publishErr   error
	clearErr     error
}

func (m *mockPlayingNowService) PublishPlayingNow(ctx context.Context, userID int64, track *models.Track) error {
	m.publishCalls = append(m.publishCalls, publishCall{userID: userID, track: track})
	return m.publishErr
}

func (m *mockPlayingNowService) ClearPlayingNow(ctx context.Context, userID int64) error {
	m.clearCalls = append(m.clearCalls, userID)
	return m.clearErr
}

// ===== Test Helpers =====

func setupTestDB(t *testing.T) *db.DB {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}

	if err := database.Initialize(); err != nil {
		t.Fatalf("Failed to initialize test database: %v", err)
	}

	return database
}

func createTestUser(t *testing.T, database *db.DB) int64 {
	user := &models.User{
		Email: func() *string { s := "test@example.com"; return &s }(),
	}
	userID, err := database.CreateUser(user)
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}
	return userID
}

func createTestTrack(name, artistName, url string, durationMs, progressMs int64) *models.Track {
	return &models.Track{
		Name:           name,
		Artist:         []models.Artist{{Name: artistName, ID: "artist123"}},
		Album:          "Test Album",
		URL:            url,
		DurationMs:     durationMs,
		ProgressMs:     progressMs,
		ServiceBaseUrl: "open.spotify.com",
		ISRC:           "TEST1234567",
		Timestamp:      time.Now().UTC(),
	}
}

func newTestService(database *db.DB, playingNow *mockPlayingNowService) *Service {
	return &Service{
		DB:                 database,
		atprotoAuthService: nil,
		mb:                 nil,
		playingNowService:  playingNow,
		userPlayStates:     make(map[int64]*userPlayState),
		userTokens:         make(map[int64]string),
		logger:             log.New(io.Discard, "", 0),
	}
}

func withUserContext(ctx context.Context, userID int64) context.Context {
	return session.WithUserID(ctx, userID)
}

// ===== getFirstArtist Tests =====

func TestGetFirstArtist(t *testing.T) {
	testCases := []struct {
		name     string
		track    *models.Track
		expected string
	}{
		{
			name:     "nil track",
			track:    nil,
			expected: "Unknown Artist",
		},
		{
			name: "empty artists",
			track: &models.Track{
				Name:   "Test Track",
				Artist: []models.Artist{},
			},
			expected: "Unknown Artist",
		},
		{
			name: "one artist",
			track: &models.Track{
				Name:   "Test Track",
				Artist: []models.Artist{{Name: "Daft Punk", ID: "123"}},
			},
			expected: "Daft Punk",
		},
		{
			name: "multiple artists",
			track: &models.Track{
				Name: "Test Track",
				Artist: []models.Artist{
					{Name: "Artist A", ID: "1"},
					{Name: "Artist B", ID: "2"},
				},
			},
			expected: "Artist A",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := getFirstArtist(tc.track)
			if result != tc.expected {
				t.Errorf("Expected '%s', got '%s'", tc.expected, result)
			}
		})
	}
}

// ===== computeStateUpdate Tests =====

func TestComputeStateUpdate_NoPriorState(t *testing.T) {
	t.Run("track playing, no prior state", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 5000)
		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}

		action := svc.computeStateUpdate(userID, resp)

		// Should publish now playing
		if !action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be true")
		}
		if action.clearNowPlaying {
			t.Error("Expected clearNowPlaying to be false")
		}

		// State should be created
		state := svc.userPlayStates[userID]
		if state == nil {
			t.Fatal("Expected state to be created")
		}
		if state.isPaused {
			t.Error("Expected isPaused to be false")
		}
		// accumulatedMs should be min(progressMs, maxSkipDeltaMs)
		if state.accumulatedMs != 5000 {
			t.Errorf("Expected accumulatedMs to be 5000, got %d", state.accumulatedMs)
		}
	})

	t.Run("track playing with high progress, capped at maxSkipDeltaMs", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		// Progress is 60s, should be capped at 30s
		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 60000)
		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}

		action := svc.computeStateUpdate(userID, resp)

		if !action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be true")
		}

		state := svc.userPlayStates[userID]
		if state.accumulatedMs != maxSkipDeltaMs {
			t.Errorf("Expected accumulatedMs to be capped at %d, got %d", maxSkipDeltaMs, state.accumulatedMs)
		}
	})

	t.Run("track paused, no prior state", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 5000)
		resp := &SpotifyTrackResponse{Track: track, IsPlaying: false}

		action := svc.computeStateUpdate(userID, resp)

		if !action.clearNowPlaying {
			t.Error("Expected clearNowPlaying to be true")
		}
		if action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be false")
		}

		state := svc.userPlayStates[userID]
		if state == nil {
			t.Fatal("Expected state to be created")
		}
		if !state.isPaused {
			t.Error("Expected isPaused to be true")
		}
	})

	t.Run("nil response", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		action := svc.computeStateUpdate(userID, nil)

		// Should be a no-op
		if action.clearNowPlaying {
			t.Error("Expected clearNowPlaying to be false for nil response with no prior state")
		}
		if action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be false")
		}
		if action.stampTrack {
			t.Error("Expected stampTrack to be false")
		}
	})

	t.Run("nil track in response", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		resp := &SpotifyTrackResponse{Track: nil, IsPlaying: true}
		action := svc.computeStateUpdate(userID, resp)

		// Should be a no-op
		if action.clearNowPlaying {
			t.Error("Expected clearNowPlaying to be false for nil track with no prior state")
		}
		if action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be false")
		}
		if action.stampTrack {
			t.Error("Expected stampTrack to be false")
		}
	})
}

func TestComputeStateUpdate_SameTrackContinues(t *testing.T) {
	t.Run("same track still playing, accumulates time", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 5000)

		// Set up existing state
		pastTime := time.Now().Add(-10 * time.Second) // 10 seconds ago
		svc.userPlayStates[userID] = &userPlayState{
			track:         track,
			accumulatedMs: 5000,
			lastPollTime:  pastTime,
			hasStamped:    false,
			isPaused:      false,
		}

		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}
		action := svc.computeStateUpdate(userID, resp)

		// Should not publish (same track continuing)
		if action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be false for same track continuing")
		}

		state := svc.userPlayStates[userID]
		// Should have added ~10s to accumulated (within tolerance)
		expectedMin := int64(5000 + 9000)  // at least 9s added
		expectedMax := int64(5000 + 11000) // at most 11s added
		if state.accumulatedMs < expectedMin || state.accumulatedMs > expectedMax {
			t.Errorf("Expected accumulatedMs between %d and %d, got %d", expectedMin, expectedMax, state.accumulatedMs)
		}
	})

	t.Run("same track now paused", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 5000)

		svc.userPlayStates[userID] = &userPlayState{
			track:         track,
			accumulatedMs: 60000,
			lastPollTime:  time.Now(),
			hasStamped:    false,
			isPaused:      false,
		}

		resp := &SpotifyTrackResponse{Track: track, IsPlaying: false}
		action := svc.computeStateUpdate(userID, resp)

		if !action.clearNowPlaying {
			t.Error("Expected clearNowPlaying to be true")
		}

		state := svc.userPlayStates[userID]
		if !state.isPaused {
			t.Error("Expected isPaused to be true")
		}
	})

	t.Run("same track resumed from pause", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 5000)

		svc.userPlayStates[userID] = &userPlayState{
			track:         track,
			accumulatedMs: 60000,
			lastPollTime:  time.Now(),
			hasStamped:    false,
			isPaused:      true, // Was paused
		}

		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}
		action := svc.computeStateUpdate(userID, resp)

		if !action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be true when resuming")
		}

		state := svc.userPlayStates[userID]
		if state.isPaused {
			t.Error("Expected isPaused to be false after resume")
		}
	})

	t.Run("delta time capped at maxDeltaMs", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 5000)

		// Set up state with lastPollTime 60 seconds ago
		pastTime := time.Now().Add(-60 * time.Second)
		svc.userPlayStates[userID] = &userPlayState{
			track:         track,
			accumulatedMs: 10000,
			lastPollTime:  pastTime,
			hasStamped:    false,
			isPaused:      false,
		}

		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}
		svc.computeStateUpdate(userID, resp)

		state := svc.userPlayStates[userID]
		// Should be capped: 10000 + 30000 = 40000 (not 10000 + 60000)
		if state.accumulatedMs > 10000+maxDeltaMs+1000 { // small tolerance
			t.Errorf("Expected delta to be capped at maxDeltaMs, got accumulatedMs=%d", state.accumulatedMs)
		}
	})
}

func TestComputeStateUpdate_NewTrackDetected(t *testing.T) {
	t.Run("different track URL", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		oldTrack := createTestTrack("Old Song", "Old Artist", "http://spotify/track1", 240000, 120000)
		newTrack := createTestTrack("New Song", "New Artist", "http://spotify/track2", 180000, 5000)

		svc.userPlayStates[userID] = &userPlayState{
			track:         oldTrack,
			accumulatedMs: 120000,
			lastPollTime:  time.Now(),
			hasStamped:    true,
			isPaused:      false,
		}

		resp := &SpotifyTrackResponse{Track: newTrack, IsPlaying: true}
		action := svc.computeStateUpdate(userID, resp)

		if !action.publishNowPlaying {
			t.Error("Expected publishNowPlaying to be true for new track")
		}

		state := svc.userPlayStates[userID]
		if state.track.URL != newTrack.URL {
			t.Error("Expected state to have new track")
		}
		if state.hasStamped {
			t.Error("Expected hasStamped to be reset to false")
		}
		if state.accumulatedMs != 5000 {
			t.Errorf("Expected accumulatedMs to be reset to progressMs (5000), got %d", state.accumulatedMs)
		}
	})
}

func TestComputeStateUpdate_SongRepeat(t *testing.T) {
	t.Run("loop detected when accumulated >= duration", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 180000, 5000)

		// Set accumulated to just under duration
		svc.userPlayStates[userID] = &userPlayState{
			track:         track,
			accumulatedMs: 175000,
			lastPollTime:  time.Now().Add(-10 * time.Second),
			hasStamped:    true,
			isPaused:      false,
		}

		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}
		svc.computeStateUpdate(userID, resp)

		state := svc.userPlayStates[userID]
		// After adding ~10s, accumulated should be ~185000, exceeding duration of 180000
		// So it should subtract duration: 185000 - 180000 = 5000 (approx)
		if state.accumulatedMs >= track.DurationMs {
			t.Errorf("Expected accumulatedMs to be reset below duration, got %d", state.accumulatedMs)
		}
		if state.hasStamped {
			t.Error("Expected hasStamped to be reset to false after loop")
		}
	})

	t.Run("overflow preserved after loop", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 100000, 5000)

		// Set accumulated to duration + 5000
		svc.userPlayStates[userID] = &userPlayState{
			track:         track,
			accumulatedMs: 105000,
			lastPollTime:  time.Now(), // recent, so delta is small
			hasStamped:    true,
			isPaused:      false,
		}

		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}
		svc.computeStateUpdate(userID, resp)

		state := svc.userPlayStates[userID]
		// Should have subtracted duration: 105000 - 100000 = 5000 (plus small delta)
		if state.accumulatedMs < 5000 || state.accumulatedMs > 6000 {
			t.Errorf("Expected accumulatedMs around 5000 after loop, got %d", state.accumulatedMs)
		}
	})
}

func TestComputeStateUpdate_StampThreshold(t *testing.T) {
	testCases := []struct {
		name          string
		durationMs    int64
		accumulatedMs int64
		hasStamped    bool
		expectStamp   bool
	}{
		{
			name:          "half duration on long track",
			durationMs:    240000, // 4 min
			accumulatedMs: 121000, // just over 2 min
			hasStamped:    false,
			expectStamp:   true,
		},
		{
			name:          "30s threshold on medium track",
			durationMs:    50000, // 50 sec track, threshold = max(25s, 30s) = 30s
			accumulatedMs: 31000, // over 30s
			hasStamped:    false,
			expectStamp:   true,
		},
		{
			name:          "below threshold",
			durationMs:    240000,
			accumulatedMs: 50000, // threshold is 120000
			hasStamped:    false,
			expectStamp:   false,
		},
		{
			name:          "already stamped",
			durationMs:    240000,
			accumulatedMs: 150000,
			hasStamped:    true,
			expectStamp:   false,
		},
		{
			name:          "exactly at threshold should not stamp",
			durationMs:    240000,
			accumulatedMs: 120000, // exactly at threshold, needs to be > threshold
			hasStamped:    false,
			expectStamp:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			database := setupTestDB(t)
			defer database.Close()

			svc := newTestService(database, nil)
			userID := int64(1)

			track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", tc.durationMs, 5000)

			svc.userPlayStates[userID] = &userPlayState{
				track:         track,
				accumulatedMs: tc.accumulatedMs,
				lastPollTime:  time.Now(), // recent, so minimal delta added
				hasStamped:    tc.hasStamped,
				isPaused:      false,
			}

			resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}
			action := svc.computeStateUpdate(userID, resp)

			if action.stampTrack != tc.expectStamp {
				t.Errorf("Expected stampTrack=%v, got %v", tc.expectStamp, action.stampTrack)
			}

			if tc.expectStamp {
				state := svc.userPlayStates[userID]
				if !state.hasStamped {
					t.Error("Expected hasStamped to be true after stamping")
				}
			}
		})
	}
}

func TestComputeStateUpdate_EdgeCases(t *testing.T) {
	t.Run("zero duration track", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 0, 0)

		resp := &SpotifyTrackResponse{Track: track, IsPlaying: true}
		action := svc.computeStateUpdate(userID, resp)

		// Should not panic, threshold should be max(0, 30000) = 30000
		if action.stampTrack {
			t.Error("Should not stamp with 0 accumulated time")
		}
	})

	t.Run("nil response with existing state clears now playing", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := int64(1)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 5000)
		svc.userPlayStates[userID] = &userPlayState{
			track:         track,
			accumulatedMs: 60000,
			lastPollTime:  time.Now(),
			hasStamped:    false,
			isPaused:      false,
		}

		action := svc.computeStateUpdate(userID, nil)

		if !action.clearNowPlaying {
			t.Error("Expected clearNowPlaying to be true when response is nil with existing state")
		}

		state := svc.userPlayStates[userID]
		if !state.isPaused {
			t.Error("Expected isPaused to be true")
		}
	})
}

// ===== HTTP Handler Tests =====

func TestHandleCurrentTrack(t *testing.T) {
	t.Run("no auth returns unauthorized", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)

		req := httptest.NewRequest(http.MethodGet, "/current", nil)
		rr := httptest.NewRecorder()

		svc.HandleCurrentTrack(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("no state returns no track playing", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := createTestUser(t, database)

		req := httptest.NewRequest(http.MethodGet, "/current", nil)
		ctx := withUserContext(req.Context(), userID)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		svc.HandleCurrentTrack(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if rr.Body.String() != "No track currently playing" {
			t.Errorf("Expected 'No track currently playing', got '%s'", rr.Body.String())
		}
	})

	t.Run("nil track in state returns no track playing", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := createTestUser(t, database)

		svc.userPlayStates[userID] = &userPlayState{
			track: nil,
		}

		req := httptest.NewRequest(http.MethodGet, "/current", nil)
		ctx := withUserContext(req.Context(), userID)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		svc.HandleCurrentTrack(rr, req)

		if rr.Body.String() != "No track currently playing" {
			t.Errorf("Expected 'No track currently playing', got '%s'", rr.Body.String())
		}
	})

	t.Run("success returns track JSON", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := createTestUser(t, database)

		track := createTestTrack("Test Song", "Test Artist", "http://spotify/track1", 240000, 60000)
		svc.userPlayStates[userID] = &userPlayState{
			track: track,
		}

		req := httptest.NewRequest(http.MethodGet, "/current", nil)
		ctx := withUserContext(req.Context(), userID)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		svc.HandleCurrentTrack(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
		}

		contentType := rr.Header().Get("Content-Type")
		if contentType != "application/json" {
			t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
		}

		var returnedTrack models.Track
		if err := json.Unmarshal(rr.Body.Bytes(), &returnedTrack); err != nil {
			t.Fatalf("Failed to parse response JSON: %v", err)
		}

		if returnedTrack.Name != "Test Song" {
			t.Errorf("Expected track name 'Test Song', got '%s'", returnedTrack.Name)
		}
	})
}

func TestHandleTrackHistory(t *testing.T) {
	t.Run("no auth returns unauthorized", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)

		req := httptest.NewRequest(http.MethodGet, "/history", nil)
		rr := httptest.NewRecorder()

		svc.HandleTrackHistory(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("empty history returns empty array", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := createTestUser(t, database)

		req := httptest.NewRequest(http.MethodGet, "/history", nil)
		ctx := withUserContext(req.Context(), userID)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		svc.HandleTrackHistory(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
		}

		var tracks []*models.Track
		if err := json.Unmarshal(rr.Body.Bytes(), &tracks); err != nil {
			t.Fatalf("Failed to parse response JSON: %v", err)
		}

		if len(tracks) != 0 {
			t.Errorf("Expected empty array, got %d tracks", len(tracks))
		}
	})

	t.Run("success returns tracks", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		userID := createTestUser(t, database)

		// Save some tracks to the database
		track1 := createTestTrack("Track 1", "Artist 1", "http://spotify/track1", 180000, 0)
		track2 := createTestTrack("Track 2", "Artist 2", "http://spotify/track2", 200000, 0)

		if _, err := database.SaveTrack(userID, track1); err != nil {
			t.Fatalf("Failed to save track1: %v", err)
		}
		if _, err := database.SaveTrack(userID, track2); err != nil {
			t.Fatalf("Failed to save track2: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/history", nil)
		ctx := withUserContext(req.Context(), userID)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		svc.HandleTrackHistory(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
		}

		contentType := rr.Header().Get("Content-Type")
		if contentType != "application/json" {
			t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
		}

		var tracks []*models.Track
		if err := json.Unmarshal(rr.Body.Bytes(), &tracks); err != nil {
			t.Fatalf("Failed to parse response JSON: %v", err)
		}

		if len(tracks) != 2 {
			t.Errorf("Expected 2 tracks, got %d", len(tracks))
		}
	})
}

// ===== stampTrack Tests =====

func TestStampTrack(t *testing.T) {
	t.Run("saves track to database with HasStamped true", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		// createTestUser does not assign a DID to the user.
		// This prevents a PDS submission from occurring.
		userID := createTestUser(t, database)

		track := createTestTrack("Stamp Test", "Test Artist", "http://spotify/track1", 240000, 0)

		svc.stampTrack(context.Background(), userID, track, 130000)

		// Verify track was saved
		tracks, err := database.GetRecentTracks(userID, 10)
		if err != nil {
			t.Fatalf("Failed to get recent tracks: %v", err)
		}

		if len(tracks) != 1 {
			t.Fatalf("Expected 1 track, got %d", len(tracks))
		}

		if tracks[0].Name != "Stamp Test" {
			t.Errorf("Expected track name 'Stamp Test', got '%s'", tracks[0].Name)
		}

		if !tracks[0].HasStamped {
			t.Error("Expected HasStamped to be true")
		}
	})

	t.Run("without MusicBrainz service saves original track", func(t *testing.T) {
		database := setupTestDB(t)
		defer database.Close()

		svc := newTestService(database, nil)
		svc.mb = nil // Explicitly nil, already should be but just in case
		userID := createTestUser(t, database)

		track := createTestTrack("No MB Test", "Test Artist", "http://spotify/track1", 240000, 0)

		svc.stampTrack(context.Background(), userID, track, 130000)

		tracks, err := database.GetRecentTracks(userID, 10)
		if err != nil {
			t.Fatalf("Failed to get recent tracks: %v", err)
		}

		if len(tracks) != 1 {
			t.Fatalf("Expected 1 track, got %d", len(tracks))
		}

		// Track should be saved even without MB service
		if tracks[0].Name != "No MB Test" {
			t.Errorf("Expected track name 'No MB Test', got '%s'", tracks[0].Name)
		}
	})
}
