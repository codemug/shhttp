package main

import "time"

type Executable struct {
	Command  string
	Args     []string
	ExecPath string
	Stdin    string
	Shell    bool
}

type ExecResult struct {
	Executable Executable
	Stdout     string
	Stderr     string
	ExitCode   int
}

type Status string

const (
	InProgress Status = "IN_PROGRESS"
	Done       Status = "DONE"
	Failed     Status = "FAILED"
)

type Job struct {
	Id           string
	Executions   []ExecResult
	Status       Status
	Created      time.Time
	LastModified time.Time
}

type IdsResponse struct {
	Ids []string
}

type JobStore interface {
	SaveNewJob(job *Job) error
	UpdateJob(job *Job) error
	GetJob(id string) (*Job, error)
	GetIds() ([]string, error)
	DeleteJob(id string) error
	ClearFinished()
}