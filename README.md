# 🍿 Memento

Visualize your Letterboxd profile data with interactive charts and insights!

## How It Works

1. Enter your Letterboxd username (or upload your data export)
2. The app scrapes your public profile data (this may take a few minutes)
3. Explore interactive visualizations across multiple categories!

| Category | Visualizations |
|----------|----------------|
| **Likes & Ratings** | Histogram • Stat tiles |
| **Diary Activity** | Calendar heatmap • Line chart • Weekday bars |
| **Genres & Themes** | Bar charts |
| **Decades** | Radar chart |
| **Obscurity Metrics** | Histogram • Rarest films list |
| **Runtime Analysis** | Bar chart |
| **People & Studios** | Bar charts |
| **Languages & Countries** | Bar charts |

## Installation & Usage

### Option 1: Run with Docker
1. Clone the repository:
   ```bash
   git clone https://github.com/danielkwan-dev/memento.git
   cd memento
   ```
2. Start everything:
   ```bash
   docker compose up
   ```
3. Visit [localhost:8080](http://localhost:8080)

### Option 2: Run Locally
1. Start Postgres:
   ```bash
   docker compose up -d postgres
   ```
2. Run the API and the worker:
   ```bash
   go run ./cmd/api
   go run ./cmd/worker
   ```
3. Run the frontend!
   ```bash
   cd web && npm install && npm run dev
   ```

## Data Privacy

- The app only accesses the **public** Letterboxd data on your profile
- Your ratings and diary entries can be deleted at any time from the dashboard
- Film details are cached and shared between users, so popular films are only fetched once

## Tech Stack
**Backend**:  
Go • Postgres • goquery

**Frontend & Visualizations**:  
React • TypeScript • Recharts • Tailwind

**Infrastructure**:  
Docker • GitHub Actions • Render • Neon

The API image also serves the built React app, so the whole web tier is one
deploy with no CORS to configure.

## Notes

A few things worth knowing if you run this yourself:

- Letterboxd has no public API, so requests go through a client that replays real browser TLS fingerprints
- Rate limits are per-IP and cumulative, so a long scrape can earn a temporary block that clears on its own
- Uploading your export is more reliable than scraping for large accounts, since it skips the paginated profile pages
- The free hosting tier sleeps when idle, so the first run after a quiet spell waits a few seconds to wake up (the UI shows this as a real 'waking' phase rather than an unexplained pause)

Running the tests:
```bash
go test ./...

docker compose up -d postgres
MEMENTO_TEST_DATABASE_URL=postgres://memento:memento@localhost:5432/memento?sslmode=disable go test ./...
```

##
Thanks for reading :)
