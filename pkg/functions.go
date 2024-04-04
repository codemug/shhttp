package pkg

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

func Execute(result *ExecResult) {
	glog.V(2).Infof("received new execution: %+v", result.Executable)
	command := getCommand(result)
	if result.Executable.Stdin != "" {
		command.Stdin = bytes.NewBufferString(result.Executable.Stdin)
	}
	if result.Executable.BaseDir != "" {
		command.Dir = result.Executable.BaseDir
	}
	if result.Executable.Env != nil {
		command.Env = updateEnv(os.Environ(), result.Executable.Env)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	result.Start = time.Now().Unix()
	if err := command.Run(); err != nil {
		glog.Error(err)
		result.Stderr = err.Error()
		result.ExitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				result.ExitCode = status.ExitStatus()
				glog.Errorf("exit Code: %d", result.ExitCode)
			}
		}
	}
	result.End = time.Now().Unix()
	result.Stdout = stdout.String()
	if result.Stderr == "" {
		result.Stderr = stderr.String()
	}
}

type FileBasedJobStore struct {
	BasePath string
}

func getCommand(result *ExecResult) *exec.Cmd {
	if result.Executable.Shell {
		args := strings.Join(result.Executable.Args, " ")
		shellCmd := strings.Join([]string{result.Executable.Command, args}, " ")
		return exec.Command("sh", "-c", shellCmd)
	}
	return exec.Command(result.Executable.Command, result.Executable.Args...)
}

func (j FileBasedJobStore) EnsureDirectory() {
	if _, err := os.Stat(j.BasePath); os.IsNotExist(err) {
		glog.V(2).Infof("creating directory: %s", j.BasePath)
		err := os.MkdirAll(j.BasePath, os.ModePerm)
		if err != nil {
			glog.Error(err)
		}
	}
}

func (j FileBasedJobStore) getFullPath(id string) string {
	return path.Join(j.BasePath, id)
}

func (j FileBasedJobStore) getNewId(job *Job) string {
	if job != nil && job.Id != "" {
		return job.Id
	}
	return strings.Join([]string{strconv.FormatInt(time.Now().UnixNano(), 10), uuid.New().String()}, "-")
}

func (j FileBasedJobStore) saveJob(job *Job) error {
	toSave, err := json.Marshal(job)
	if err != nil {
		glog.Error(err)
		return err
	}
	if err = ioutil.WriteFile(j.getFullPath(job.Id), toSave, os.ModePerm); err != nil {
		glog.Error(err)
		return err
	}
	glog.V(2).Infof("Job saved: %+v", *job)
	return nil
}

func (j FileBasedJobStore) loadJob(path string) (*Job, error) {
	read, err := ioutil.ReadFile(path)
	if err != nil {
		glog.Error(err)
		return nil, err
	}
	var job Job
	err = json.Unmarshal(read, &job)
	if err != nil {
		glog.Error(err)
		return nil, err
	}
	return &job, nil
}

func (j FileBasedJobStore) SaveNewJob(job *Job) error {
	j.EnsureDirectory()
	job.Id = j.getNewId(job)
	return j.saveJob(job)
}

func (j FileBasedJobStore) UpdateJob(job *Job) error {
	j.EnsureDirectory()
	if _, err := os.Stat(j.getFullPath(job.Id)); err == nil {

		return j.saveJob(job)
	} else if os.IsNotExist(err) {
		return errors.New("the job with the specified id does not exist")
	} else {
		glog.Error(err)
		return err
	}
}

func (j FileBasedJobStore) GetJob(id string) (*Job, error) {
	j.EnsureDirectory()
	fullPath := j.getFullPath(id)
	if _, err := os.Stat(fullPath); err == nil {
		return j.loadJob(fullPath)
	} else if os.IsNotExist(err) {
		return nil, errors.New("the job with the specified id does not exist")
	} else {
		glog.Error(err)
		return nil, err
	}
}

func (j FileBasedJobStore) GetIds() ([]string, error) {
	j.EnsureDirectory()
	files, err := ioutil.ReadDir(j.BasePath)
	if err != nil {
		glog.Error(err)
		return nil, err
	}
	ids := make([]string, len(files))
	for i, v := range files {
		ids[i] = v.Name()
	}
	return ids, nil
}

func (j FileBasedJobStore) ClearFinished() {
	j.EnsureDirectory()
	files, err := ioutil.ReadDir(j.BasePath)
	if err != nil {
		glog.Error(err)
		return
	}
	for _, v := range files {
		if !v.IsDir() {
			job, err := j.loadJob(j.getFullPath(v.Name()))
			if err != nil {
				glog.Error(err)
				continue
			}
			if job.Status != InProgress {
				glog.V(2).Infof("Cleaning up finished job: %+v", job)
				err := os.Remove(j.getFullPath(v.Name()))
				if err != nil {
					glog.Error(err)
					continue
				}
			}
		}
	}
}

func (j FileBasedJobStore) DeleteJob(id string) error {
	j.EnsureDirectory()
	if _, err := os.Stat(j.getFullPath(id)); err == nil {
		return os.Remove(j.getFullPath(id))
	} else if os.IsNotExist(err) {
		return errors.New("the job with the specified id does not exist")
	} else {
		glog.Error(err)
		return err
	}
}

func GetRouter(jobStore JobStore, savedJobStore JobStore, revive bool, password string) *mux.Router {
	router := mux.NewRouter()

	jobQueue := make(chan *Job)

	go func() {
		for {
			job := <-jobQueue
			ExecuteJob(job, jobStore)
		}
	}()

	go func() {
		ids, err := jobStore.GetIds()
		if err != nil {
			glog.Error(err)
			return
		}
		for _, id := range ids {
			job, err := jobStore.GetJob(id)
			if err != nil {
				glog.Error(err)
				continue
			}
			if revive {
				if job.Status == InProgress {
					glog.V(2).Infof("reviving independent job %+v", *job)
					go ExecuteJob(job, jobStore)
				} else if job.Status == Queued {
					glog.V(2).Infof("queueing job for revival %+v", *job)
					go func() {
						jobQueue <- job
					}()
				}
			} else {
				glog.V(2).Infof("job revival disabled, failing existing job %+v", *job)
				job.Status = Failed
				jobStore.UpdateJob(job)
			}
		}
	}()

	getIds := func(writer http.ResponseWriter, request *http.Request, store JobStore) {
		writeContentType(writer)
		ids, err := store.GetIds()
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
			return
		}
		response := IdsResponse{Ids: ids}
		dataInBytes, err := json.Marshal(response)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
			return
		}
		writer.Write(dataInBytes)
	}

	getJob := func(writer http.ResponseWriter, request *http.Request, store JobStore) {
		writeContentType(writer)
		vars := mux.Vars(request)
		id := vars["id"]
		job, err := store.GetJob(id)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusNotFound, writer)
			return
		}
		jobBytes, err := json.Marshal(job)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
			return
		}
		writer.Write(jobBytes)
	}

	runJob := func(writer http.ResponseWriter, request *http.Request, job *Job, queued bool) {
		writeContentType(writer)
		job.Created = time.Now().Unix()
		if queued {
			job.Status = Queued
		} else {
			job.Status = InProgress
		}
		err := jobStore.SaveNewJob(job)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
			return
		}
		writer.Write([]byte(getIdResponse(job.Id)))
		if queued {
			go func() {
				jobQueue <- job
			}()
		} else {
			go ExecuteJob(job, jobStore)
		}
	}

	router.Path("/v1/exec").Methods(http.MethodPost).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeContentType(writer)
		var executable Executable
		err := json.NewDecoder(request.Body).Decode(&executable)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusBadRequest, writer)
			return
		}
		if executable.Password != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		// Omit the "Password" field when marshaling if any.
		executable.Password = ""
		result := ExecResult{Executable: &executable}
		Execute(&result)
		respData, err := json.Marshal(result)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
		} else {
			writer.Write(respData)
		}
	})

	router.Path("/v1/jobs").Methods(http.MethodPost).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var job Job
		err := json.NewDecoder(request.Body).Decode(&job)
		q := request.URL.Query()
		queue := strings.ToLower(q.Get("queue")) == "true"
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusBadRequest, writer)
			return
		}
		if job.Password != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		// Omit the "Password" field when marshaling if any.
		job.Password = ""
		runJob(writer, request, &job, queue)
	})

	router.Path("/v1/jobs").Methods(http.MethodGet).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		q := request.URL.Query()
		if q.Get("password") != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		getIds(writer, request, jobStore)
	})

	router.Path("/v1/jobs/{id}").Methods(http.MethodGet).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		q := request.URL.Query()
		if q.Get("password") != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		getJob(writer, request, jobStore)
	})

	router.Path("/v1/saved").Methods(http.MethodGet).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		q := request.URL.Query()
		if q.Get("password") != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		getIds(writer, request, savedJobStore)
	})

	router.Path("/v1/saved").Methods(http.MethodPut).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeContentType(writer)
		var job Job
		err := json.NewDecoder(request.Body).Decode(&job)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusBadRequest, writer)
			return
		}
		if job.Password != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		// Omit the "Password" field when marshaling if any.
		job.Password = ""
		if job.Id == "" {
			job.Id = uuid.New().String()
		}
		savedJobStore.SaveNewJob(&job)
		writer.WriteHeader(http.StatusCreated)
		writer.Write([]byte(getIdResponse(job.Id)))
	})

	router.Path("/v1/saved/{id}").Methods(http.MethodDelete).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeContentType(writer)
		q := request.URL.Query()
		if q.Get("password") != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		vars := mux.Vars(request)
		id := vars["id"]
		err := savedJobStore.DeleteJob(id)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
		}
		writer.WriteHeader(http.StatusNoContent)
	})

	router.Path("/v1/saved/{id}").Methods(http.MethodGet).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		q := request.URL.Query()
		if q.Get("password") != password {
			err := errors.New("password invalid");
			glog.Error(err)
			writeErrorResponse(err, http.StatusUnauthorized, writer)
			return
		}
		getJob(writer, request, savedJobStore)
	})

	router.Path("/v1/saved/{id}").Methods(http.MethodPost).HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		vars := mux.Vars(request)
		id := vars["id"]
		q := request.URL.Query()
		queue := strings.ToLower(q.Get("queue")) == "true"

		job, err := savedJobStore.GetJob(id)
		job.Id = ""
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusNotFound, writer)
			return
		}
		var runBody RunBody
		err = json.NewDecoder(request.Body).Decode(&runBody)
		if err != nil {
			glog.Error(err)
		} else {
			if runBody.Password != password {
				err := errors.New("password invalid");
				glog.Error(err)
				writeErrorResponse(err, http.StatusUnauthorized, writer)
				return
			}
			// Omit the "Password" field when marshaling if any.
			runBody.Password = ""
			updateJobEnv(job, runBody.Env)
		}
		runJob(writer, request, job, queue)
	})

	return router
}

func writeErrorResponse(err error, status int, writer http.ResponseWriter) {
	writer.WriteHeader(status)
	writer.Write([]byte(strings.Join([]string{"{\"error\": \"", err.Error(), "\"}"}, "")))
}

func getIdResponse(id string) string {
	return strings.Join([]string{"{\"id\": \"", id, "\"}"}, "")
}

func writeContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
}

func updateEnv(existingEnv []string, newVars map[string]string) []string {
	strArray := make([]string, len(newVars))
	i := 0
	for k, v := range newVars {
		strArray[i] = strings.Join([]string{k, v}, "=")
	}
	return append(existingEnv, strArray...)
}

func ExecuteJob(job *Job, jobStore JobStore) {
	if job.Executions != nil {
		for _, execution := range job.Executions {
			if execution.Executable != nil {
				Execute(execution)
				job.LastModified = time.Now().Unix()
				if execution.ExitCode != 0 && !job.IgnoreErrors {
					job.Status = Failed
					jobStore.UpdateJob(job)
					return
				}
				jobStore.UpdateJob(job)
			}
		}
	}
	job.LastModified = time.Now().Unix()
	job.Status = Done
	jobStore.UpdateJob(job)
}

func updateJobEnv(job *Job, newEnvs map[string]string) {
	for _, v := range job.Executions {
		for key, value := range newEnvs {
			v.Executable.Env[key] = value
		}
	}
}
