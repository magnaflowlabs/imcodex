package main

import "testing"

func TestBuildScheduledJobsInheritsGroupSessionCommand(t *testing.T) {
	t.Parallel()

	jobs := buildScheduledJobs([]groupConfig{{
		GroupID:        "oc_1",
		CWD:            "/srv/demo",
		SessionCommand: "/usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex",
		Jobs: []jobConfig{{
			Name:       "hourly_review",
			Schedule:   "1 * * * *",
			PromptFile: "/srv/demo/prompts/hourly_review.md",
		}},
	}})
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if got, want := jobs[0].SessionCommand, "/usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex"; got != want {
		t.Fatalf("job session_command = %q, want %q", got, want)
	}
}

func TestBuildScheduledJobsPrefersJobSessionOverrides(t *testing.T) {
	t.Parallel()

	jobs := buildScheduledJobs([]groupConfig{{
		GroupID:        "oc_1",
		CWD:            "/srv/demo",
		SessionCommand: "group-command",
		Jobs: []jobConfig{{
			Name:           "hourly_review",
			Schedule:       "1 * * * *",
			PromptFile:     "/srv/demo/prompts/hourly_review.md",
			SessionName:    "job-session",
			SessionCommand: "job-command",
		}},
	}})
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if got, want := jobs[0].SessionName, "job-session"; got != want {
		t.Fatalf("job session_name = %q, want %q", got, want)
	}
	if got, want := jobs[0].SessionCommand, "job-command"; got != want {
		t.Fatalf("job session_command = %q, want %q", got, want)
	}
}
