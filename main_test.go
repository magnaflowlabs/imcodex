package main

import "testing"

func TestBuildScheduledJobsUsesProvidedLaunchCommand(t *testing.T) {
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
	}, "exec '/srv/imcodex/imcodex' 'internal-run-docker-codex'")
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if got, want := jobs[0].LaunchCommand, "exec '/srv/imcodex/imcodex' 'internal-run-docker-codex'"; got != want {
		t.Fatalf("job launch_command = %q, want %q", got, want)
	}
}

func TestBuildScheduledJobsKeepsJobSessionNameOverride(t *testing.T) {
	t.Parallel()

	jobs := buildScheduledJobs(config{
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
	}, "global-command")
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if got, want := jobs[0].SessionName, "job-session"; got != want {
		t.Fatalf("job session_name = %q, want %q", got, want)
	}
	if got, want := jobs[0].LaunchCommand, "global-command"; got != want {
		t.Fatalf("job launch_command = %q, want %q", got, want)
	}
}

func TestBuildScheduledJobsLeavesLaunchCommandEmptyForHostCodex(t *testing.T) {
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
	}, "")
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if jobs[0].LaunchCommand != "" {
		t.Fatalf("job launch_command = %q, want empty host default", jobs[0].LaunchCommand)
	}
}
