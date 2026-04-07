package main

import "testing"

func TestBuildScheduledJobsUsesGlobalSessionCommand(t *testing.T) {
	t.Parallel()

	jobs := buildScheduledJobs(config{
		sessionCommand: "/usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex",
		groups: []groupConfig{{
			GroupID: "oc_1",
			CWD:     "/srv/demo",
			Jobs: []jobConfig{{
				Name:       "hourly_review",
				Schedule:   "1 * * * *",
				PromptFile: "/srv/demo/prompts/hourly_review.md",
			}},
		}},
	})
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if got, want := jobs[0].SessionCommand, "/usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent codex"; got != want {
		t.Fatalf("job session_command = %q, want %q", got, want)
	}
}

func TestBuildScheduledJobsKeepsJobSessionNameOverride(t *testing.T) {
	t.Parallel()

	jobs := buildScheduledJobs(config{
		sessionCommand: "global-command",
		groups: []groupConfig{{
			GroupID: "oc_1",
			CWD:     "/srv/demo",
			Jobs: []jobConfig{{
				Name:        "hourly_review",
				Schedule:    "1 * * * *",
				PromptFile:  "/srv/demo/prompts/hourly_review.md",
				SessionName: "job-session",
			}},
		}},
	})
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if got, want := jobs[0].SessionName, "job-session"; got != want {
		t.Fatalf("job session_name = %q, want %q", got, want)
	}
	if got, want := jobs[0].SessionCommand, "global-command"; got != want {
		t.Fatalf("job session_command = %q, want %q", got, want)
	}
}

func TestBuildScheduledJobsLeavesSessionCommandEmptyForLegacyMode(t *testing.T) {
	t.Parallel()

	jobs := buildScheduledJobs(config{
		groups: []groupConfig{{
			GroupID: "oc_1",
			CWD:     "/srv/demo",
			Jobs: []jobConfig{{
				Name:       "hourly_review",
				Schedule:   "1 * * * *",
				PromptFile: "/srv/demo/prompts/hourly_review.md",
			}},
		}},
	})
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if jobs[0].SessionCommand != "" {
		t.Fatalf("job session_command = %q, want empty legacy default", jobs[0].SessionCommand)
	}
}
