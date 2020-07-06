package pkg

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestFileBasedJobStore_SaveNewJob(t *testing.T) {
	job := Job{
		Executions: []*ExecResult{{Executable: &Executable{Command: "echo", Args: []string{"abc"}}}},
		Status:     InProgress,
	}
	store := FileBasedJobStore{BasePath: "testing"}
	store.SaveNewJob(&job)
	assert.NotEmpty(t, job.Id)
	savedJob, err := store.GetJob(job.Id)
	assert.Nil(t, err)
	assert.Equal(t, savedJob.Status, job.Status)
	assert.Equal(t, len(savedJob.Executions), len(job.Executions))
	assert.Equal(t, savedJob.Executions[0].Executable.Command, job.Executions[0].Executable.Command)
	store.DeleteJob(job.Id)
	_, err = store.GetJob(job.Id)
	assert.NotNil(t, err)
}

func TestFileBasedJobStore_UpdateJob(t *testing.T) {
	job := Job{
		Executions: []*ExecResult{{Executable: &Executable{Command: "echo", Args: []string{"abc"}}}},
		Status:     InProgress,
	}
	store := FileBasedJobStore{BasePath: "testing"}
	store.SaveNewJob(&job)
	job.Status = Done
	store.UpdateJob(&job)
	savedJob, err := store.GetJob(job.Id)
	assert.Nil(t, err)
	assert.Equal(t, savedJob.Status, job.Status)
	store.DeleteJob(job.Id)
}

func TestFileBasedJobStore_GetIds(t *testing.T) {
	job := Job{
		Executions: []*ExecResult{{Executable: &Executable{Command: "echo", Args: []string{"abc"}}}},
		Status:     InProgress,
	}
	store := FileBasedJobStore{BasePath: "testing"}
	store.SaveNewJob(&job)
	ids, err := store.GetIds()
	assert.Nil(t, err)
	assert.Equal(t, len(ids), 1)
	assert.Equal(t, ids[0], job.Id)
	for _, v := range ids {
		store.DeleteJob(v)
	}
}

func TestFileBasedJobStore_ClearFinished(t *testing.T) {
	job1 := Job{
		Executions: []*ExecResult{{Executable: &Executable{Command: "echo", Args: []string{"abc"}}}},
		Status:     Done,
	}
	job2 := Job{
		Executions: []*ExecResult{{Executable: &Executable{Command: "echo", Args: []string{"abc"}}}},
		Status:     InProgress,
	}
	store := FileBasedJobStore{BasePath: "testing"}
	store.SaveNewJob(&job1)
	store.SaveNewJob(&job2)
	store.ClearFinished()
	ids, err := store.GetIds()
	assert.Nil(t, err)
	assert.Equal(t, len(ids), 1)
	assert.Equal(t, ids[0], job2.Id)
	for _, v := range ids {
		store.DeleteJob(v)
	}
}

func TestFileBasedJobStore_UpdateJob_NotExist(t *testing.T) {
	store := FileBasedJobStore{BasePath: "testing"}
	job := Job{
		Id:         store.getNewId(nil),
		Executions: []*ExecResult{{Executable: &Executable{Command: "echo", Args: []string{"abc"}}}},
		Status:     InProgress,
	}
	err := store.UpdateJob(&job)
	assert.NotNil(t, err)
}

func TestFileBasedJobStore_DeleteJob_NotExist(t *testing.T) {
	store := FileBasedJobStore{BasePath: "testing"}
	err := store.DeleteJob(store.getNewId(nil))
	assert.NotNil(t, err)
}

func TestFileBasedJobStore_GetJob_NotExist(t *testing.T) {
	store := FileBasedJobStore{BasePath: "testing"}
	_, err := store.GetJob(store.getNewId(nil))
	assert.NotNil(t, err)
}

func TestExecute(t *testing.T) {
	toExecute := ExecResult{Executable: &Executable{
		Command: "echo",
		Args:    []string{"\"Will the real slim shady please stand up\""},
	}}
	Execute(&toExecute)
	assert.Equal(t, toExecute.Stdout, "\"Will the real slim shady please stand up\"\n")
	assert.Equal(t, toExecute.ExitCode, 0)
}

func TestExecute_NotExist(t *testing.T) {
	toExecute := ExecResult{Executable: &Executable{
		Command: "echoecho",
		Args:    []string{"\"Will the real slim shady please stand up\""},
	}}
	Execute(&toExecute)
	assert.Equal(t, toExecute.Stderr, "exec: \"echoecho\": executable file not found in $PATH")
}

func TestExecute_Shell(t *testing.T) {
	toExecute := ExecResult{Executable: &Executable{
		Command: "echo",
		Args:    []string{"\"winner\nwinner\nchicken\ndinner\"", "|", "grep", "chicken"},
		Shell:   true,
	}}
	Execute(&toExecute)
	assert.Equal(t, toExecute.Stdout, "chicken\n")
	assert.Equal(t, toExecute.ExitCode, 0)
}

func TestExecute_ShellError(t *testing.T) {
	toExecute := ExecResult{Executable: &Executable{
		Command: "echo",
		Args:    []string{"winner\nwinner\nchicken\ndinner", "|", "grep", "chicken"},
		Shell:   true,
	}}
	Execute(&toExecute)
	assert.Equal(t, toExecute.ExitCode, 1)
}