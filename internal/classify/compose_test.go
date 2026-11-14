package classify

import (
	"slices"
	"strings"
	"testing"
)

// GOV-B30. Every `docker compose` call used to classify Heavy, whatever its subcommand: the
// EAR-11 session was charged 3.75 of four slots for `docker compose exec db psql`. Compose is
// classified by its own subcommand now, and the classes and the measurement behind them are
// in the heavySub note in classify.go.

// TestComposeControlPlaneIsNotGated is clause 1: each of these is a bounded lifecycle or query
// operation, measured at well under a core-second of Docker VM time, and none takes a lease.
func TestComposeControlPlaneIsNotGated(t *testing.T) {
	for _, cmd := range []string{
		// The EAR-11 shape, exactly as the session ran it, and its siblings from the corpus.
		`docker compose exec -T db psql -U earbrief -d earbrief -c "select id, status from episodes"`,
		`docker compose exec db psql -U postgres`,
		`docker compose exec -T db psql -U earbrief -tAc "select 1"`,
		"docker compose ps",
		"docker compose ps db 2>&1 | tail -2",
		"docker compose ps --format '{{.Name}} {{.Status}}'",
		"docker compose logs db",
		"docker compose logs -f --tail 50 api",
		"docker compose down",
		"docker compose down -v --remove-orphans",
		"docker compose stop",
		"docker compose stop db",
		"docker compose config",
		"docker compose config --services",
		"docker compose ls",
		"docker compose top",
		"docker compose port db 5432",
		"docker compose images",
		"docker compose kill",
		"docker compose rm -f",
		"docker compose pause db",
		"docker compose unpause db",
		// Not in clause 1's list, measured the same: start is the other half of stop.
		"docker compose start db",
		"docker compose restart api",
		"docker compose version",
		"docker compose",
		// Compose's global flags come before the subcommand, and several take a value. Before
		// this, `-f x.yml` made `x.yml` the subcommand.
		"docker compose -f docker-compose.box.yml exec -T db psql -c 'select 1'",
		"docker compose -f a.yml -f b.yml --profile tools ps",
		"docker compose -p myproj --env-file .env.test logs api",
		"docker compose --project-directory ../app --ansi never down",
		"docker compose --file=x.yml config",
		// docker's own global flags, and the standalone v1 binary.
		"docker --context prod compose ps",
		"docker -H ssh://box compose logs api",
		"docker-compose ps",
		"docker-compose -f x.yml exec db psql",
		"/usr/local/bin/docker-compose down",
		// exec runs whatever it is given, and these are not builds.
		"docker compose exec api sh -lc 'ls /app'",
		"docker compose exec -e FOO=bar -u root -w /app api cat /etc/os-release",
	} {
		if got := Classify(cmd); got.Gated {
			t.Errorf("Classify(%q) = gated %s %v, want ungated", cmd, got.Class, got.Shapes)
		}
	}
}

// TestComposeBuildsStayGated is clause 2: what builds stays Heavy, and up/run/create take the
// measured Medium rather than either extreme.
func TestComposeBuildsStayGated(t *testing.T) {
	for _, tc := range []struct {
		cmd    string
		want   Class
		shapes []string
	}{
		{"docker compose build", Heavy, []string{"docker compose build"}},
		{"docker compose build --no-cache api", Heavy, []string{"docker compose build"}},
		{"docker compose -f x.yml --profile build build builder", Heavy, []string{"docker compose build"}},
		{"docker-compose build", Heavy, []string{"docker compose build"}},
		{"docker compose up --build", Heavy, []string{"docker compose build", "docker compose up"}},
		{"docker compose up -d --build --wait api", Heavy, []string{"docker compose build", "docker compose up"}},
		{"docker compose watch", Heavy, []string{"docker compose watch"}},
		{"docker compose run --build --rm api true", Heavy, []string{"docker compose run", "docker compose build"}},

		// Measured Medium: these can pull and build a missing image with no --build flag.
		{"docker compose up -d", Medium, []string{"docker compose up"}},
		{"docker compose up -d --wait db >/dev/null 2>&1", Medium, []string{"docker compose up"}},
		{"docker compose -f docker-compose.yml up -d db", Medium, []string{"docker compose up"}},
		{"docker compose create", Medium, []string{"docker compose create"}},
		{"docker compose pull", Medium, []string{"docker compose pull"}},
		{"docker compose run --rm db psql --version", Medium, []string{"docker compose run"}},
		{"docker compose run --rm -e X=1 -v $PWD:/w api alembic upgrade head", Medium, []string{"docker compose run"}},

		// The command inside the container is what costs, as with `uv run pytest`.
		{"docker compose exec api pytest -q", Medium, []string{"pytest"}},
		{"docker compose exec -T api python -m pytest tests/", Medium, []string{"pytest"}},
		{"docker compose exec -w /src api make check", Heavy, []string{"make check"}},
		{"docker compose run --rm api go test ./...", Medium, []string{"docker compose run", "go test"}},

		// An unrecognised subcommand is Medium: not free, and not the Heavy that was the bug.
		{"docker compose scale api=3", Medium, []string{"docker compose"}},
	} {
		got := Classify(tc.cmd)
		if !got.Gated || got.Class != tc.want {
			t.Errorf("Classify(%q) = gated=%v %s, want gated %s", tc.cmd, got.Gated, got.Class, tc.want)
			continue
		}
		if !slices.Equal(got.Shapes, tc.shapes) {
			t.Errorf("Classify(%q).Shapes = %v, want %v", tc.cmd, got.Shapes, tc.shapes)
		}
	}
}

// TestComposeShapesCarryNoProjectStrings holds the trace rule for the new path: a compose
// subcommand slot can hold a service or project name when the verb is not one we know, and
// that must not reach disk.
func TestComposeShapesCarryNoProjectStrings(t *testing.T) {
	for _, c := range []struct{ cmd, secret string }{
		{"docker compose -p AcmeSecretProject up -d", "AcmeSecretProject"},
		{"docker compose -f acme-secret.yml build", "acme-secret"},
		{"docker compose frobnicate-acme-internal", "frobnicate-acme-internal"},
		{"docker compose run --rm acme-billing-worker go test ./...", "acme-billing-worker"},
	} {
		for _, s := range Classify(c.cmd).Shapes {
			if strings.Contains(s, c.secret) {
				t.Errorf("Classify(%q).Shapes carries %q", c.cmd, c.secret)
			}
		}
	}
}

// TestDockerOutsideComposeIsUnchanged pins the docker rows this change routes around.
func TestDockerOutsideComposeIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		cmd   string
		gated bool
		want  Class
	}{
		{"docker build -t x .", true, Heavy},
		{"docker run --rm alpine true", true, Medium},
		{"docker pull postgres:16", true, Medium},
		{"docker ps", false, Light},
		{"docker images", false, Light},
	} {
		got := Classify(tc.cmd)
		if got.Gated != tc.gated || got.Class != tc.want {
			t.Errorf("Classify(%q) = gated=%v %s, want gated=%v %s", tc.cmd, got.Gated, got.Class, tc.gated, tc.want)
		}
	}
}
