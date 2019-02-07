# shhttp

It's a small HTTP service written in go that can accept different commands and shell scripts, execute them on the machine it's running on and return the result. It can also manage these commands/shell scripts as jobs. When a command(s)/script(s) is submitted as a job, the response of the HTTP request is returned immediately while the command/script keeps on executing. The service also exposes api to query the state of these jobs. 


### Installation

You need to install golang and [dep](https://github.com/golang/dep) on your machine. Clone the repo in your gopath/src and do a:
```sh
dep ensure
go install
```

### Usage

The service can be directly launched from the shell, do an `sshttp --help`:

```
$ shhttp --help
Usage of shhttp:
  -alsologtostderr
    	log to standard error as well as files
  -clean-interval int
    	interval (hours) after which finished jobs are cleaned (default -1)
  -dir string
    	location to store the job data (default "shhttp")
  -hostname string
    	hostname to listen on (default "0.0.0.0")
  -log_backtrace_at value
    	when logging hits line file:N, emit a stack trace
  -log_dir string
    	If non-empty, write log files in this directory
  -logtostderr
    	log to standard error instead of files
  -port int
    	port to listen on (default 2112)
  -stderrthreshold value
    	logs at or above this threshold go to stderr
  -v value
    	log level for V logs
  -vmodule value
    	comma-separated list of pattern=N settings for file-filtered logging
```

