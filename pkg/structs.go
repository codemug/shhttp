package pkg

type Executable struct {
	Command     string
	Args        []string
	BaseDir     string
	Stdin       string
	Shell       bool
	Env         map[string]string
	Password    string `json:"Password,omitempty"`
}

type ExecResult struct {
	Executable *Executable
	Stdout     string
	Stderr     string
	ExitCode   int
	Start      int64
	End        int64
}

type Status string

const (
	InProgress Status = "IN_PROGRESS"
	Done       Status = "DONE"
	Queued     Status = "QUEUED"
	Failed     Status = "FAILED"
)

type Job struct {
	Id           string
	Executions   []*ExecResult
	Status       Status
	Created      int64
	LastModified int64
	IgnoreErrors bool
	Password     string `json:"Password,omitempty"`
}

type RunBody struct {
	Env          map[string]string
	Password     string `json:"Password,omitempty"`
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
