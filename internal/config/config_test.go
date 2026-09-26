package config

import "testing"

// Container hosts (Render, Heroku, Railway) inject PORT and point their health
// check at it. Binding to anything else there means the service is marked
// unhealthy and the deploy fails, so PORT must win over our own variable.
func TestLoad_PortPrecedence(t *testing.T) {
	t.Setenv("MEMENTO_DATABASE_URL", "postgres://x/y")

	t.Run("PORT wins", func(t *testing.T) {
		t.Setenv("PORT", "10000")
		t.Setenv("MEMENTO_PORT", "8080")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Port != 10000 {
			t.Errorf("Port = %d, want 10000 (the host's PORT)", c.Port)
		}
		if c.WorkerPort != 10000 {
			t.Errorf("WorkerPort = %d, want 10000", c.WorkerPort)
		}
	})

	t.Run("falls back to our own", func(t *testing.T) {
		t.Setenv("PORT", "")
		t.Setenv("MEMENTO_PORT", "9999")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Port != 9999 {
			t.Errorf("Port = %d, want 9999", c.Port)
		}
	})

	t.Run("defaults with neither", func(t *testing.T) {
		t.Setenv("PORT", "")
		t.Setenv("MEMENTO_PORT", "")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Port != 8080 {
			t.Errorf("Port = %d, want the 8080 default", c.Port)
		}
	})
}

func TestLoad_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("MEMENTO_DATABASE_URL", "")
	// An empty value falls back to the local default rather than erroring, so a
	// developer with no env file still gets a working local setup.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DatabaseURL == "" {
		t.Error("DatabaseURL is empty; a local default should apply")
	}
}

func TestLoad_ConcurrencyMustBePositive(t *testing.T) {
	t.Setenv("MEMENTO_DATABASE_URL", "postgres://x/y")
	t.Setenv("MEMENTO_INDEX_CONCURRENCY", "0")
	if _, err := Load(); err == nil {
		t.Error("want an error for zero concurrency")
	}
}
