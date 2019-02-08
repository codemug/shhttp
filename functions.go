package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
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

func (j FileBasedJobStore) ensureDirectory() {
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
	j.ensureDirectory()
	job.Id = j.getNewId(job)
	return j.saveJob(job)
}

func (j FileBasedJobStore) UpdateJob(job *Job) error {
	j.ensureDirectory()
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
	j.ensureDirectory()
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
	j.ensureDirectory()
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
	j.ensureDirectory()
	files, err := ioutil.ReadDir(j.BasePath)
	if err != nil {
		glog.Error(err)
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
	j.ensureDirectory()
	if _, err := os.Stat(j.getFullPath(id)); err == nil {
		return os.Remove(j.getFullPath(id))
	} else if os.IsNotExist(err) {
		return errors.New("the job with the specified id does not exist")
	} else {
		glog.Error(err)
		return err
	}
}

func GetRouter(jobStore JobStore, savedJobStore JobStore) (*mux.Router) {
	router := mux.NewRouter()

	router.HandleFunc("/v1/exec", func(writer http.ResponseWriter, request *http.Request) {
		writeContentType(writer)
		var executable Executable
		err := json.NewDecoder(request.Body).Decode(&executable)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusBadRequest, writer)
			return
		}
		result := ExecResult{Executable: &executable}
		Execute(&result)
		respData, err := json.Marshal(result)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
		} else {
			writer.Write(respData)
		}
	}).Methods(http.MethodPost)

	router.HandleFunc("/v1/jobs", func(writer http.ResponseWriter, request *http.Request) {
		var job Job
		err := json.NewDecoder(request.Body).Decode(&job)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusBadRequest, writer)
			return
		}
		runJob(writer, request, &job, jobStore)
	}).Methods(http.MethodPost)

	router.HandleFunc("/v1/jobs", func(writer http.ResponseWriter, request *http.Request) {
		getIds(writer, request, jobStore)
	}).Methods(http.MethodGet)

	router.HandleFunc("/v1/jobs/{id}", func(writer http.ResponseWriter, request *http.Request) {
		getJob(writer, request, jobStore)
	}).Methods(http.MethodGet)

	router.HandleFunc("/v1/saved", func(writer http.ResponseWriter, request *http.Request) {
		getIds(writer, request, savedJobStore)
	}).Methods(http.MethodGet)

	router.HandleFunc("/v1/saved", func(writer http.ResponseWriter, request *http.Request) {
		writeContentType(writer)
		var job Job
		err := json.NewDecoder(request.Body).Decode(&job)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusBadRequest, writer)
			return
		}
		if job.Id == "" {
			job.Id = uuid.New().String()
		}
		savedJobStore.SaveNewJob(&job)
		writer.WriteHeader(http.StatusCreated)
		writer.Write([]byte(getIdResponse(job.Id)))
	}).Methods(http.MethodPut)

	router.HandleFunc("/v1/saved/{id}", func(writer http.ResponseWriter, request *http.Request) {
		writeContentType(writer)
		vars := mux.Vars(request)
		id := vars["id"]
		err := savedJobStore.DeleteJob(id)
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusInternalServerError, writer)
		}
		writer.WriteHeader(http.StatusNoContent)
	}).Methods(http.MethodDelete)

	router.HandleFunc("/v1/saved/{id}", func(writer http.ResponseWriter, request *http.Request) {
		getJob(writer, request, jobStore)
	}).Methods(http.MethodGet)

	router.HandleFunc("/v1/saved/{id}", func(writer http.ResponseWriter, request *http.Request) {
		vars := mux.Vars(request)
		id := vars["id"]

		job, err := savedJobStore.GetJob(id)
		job.Id = ""
		if err != nil {
			glog.Error(err)
			writeErrorResponse(err, http.StatusNotFound, writer)
			return
		}
		var env Env
		err = json.NewDecoder(request.Body).Decode(&env)
		if err != nil {
			glog.Error(err)
		} else {
			for _, v := range job.Executions {
				for key, value := range env.Env {
					v.Executable.Env[key] = value
				}
			}
		}
		runJob(writer, request, job, jobStore)
	}).Methods(http.MethodPost)

	return router
}

func getIds(writer http.ResponseWriter, request *http.Request, jobStore JobStore) {
	writeContentType(writer)
	ids, err := jobStore.GetIds()
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

func getJob(writer http.ResponseWriter, request *http.Request, jobStore JobStore) {
	writeContentType(writer)
	vars := mux.Vars(request)
	id := vars["id"]
	job, err := jobStore.GetJob(id)
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

func runJob(writer http.ResponseWriter, request *http.Request, job *Job, jobStore JobStore) {
	writeContentType(writer)
	job.Created = time.Now().Unix()
	job.Status = InProgress
	err := jobStore.SaveNewJob(job)
	if err != nil {
		glog.Error(err)
		writeErrorResponse(err, http.StatusInternalServerError, writer)
		return
	}
	writer.Write([]byte(getIdResponse(job.Id)))
	go func() {
		for _, execution := range job.Executions {
			Execute(execution)
			job.LastModified = time.Now().Unix()
			if execution.ExitCode != 0 && !job.IgnoreErrors {
				job.Status = Failed
				jobStore.UpdateJob(job)
				return
			}
			jobStore.UpdateJob(job)
		}
		job.Status = Done
		jobStore.UpdateJob(job)
	}()
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
