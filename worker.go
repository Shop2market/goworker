package goworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type worker struct {
	process
}

func newWorker(id string, queues []string) (*worker, error) {
	process, err := newProcess(id, queues)
	if err != nil {
		return nil, err
	}
	return &worker{
		process: *process,
	}, nil
}

func (w *worker) MarshalJSON() ([]byte, error) {
	return json.Marshal(w.String())
}

func (w *worker) start(conn *RedisConn, job *Job) error {
	work := &work{
		Queue:   job.Queue,
		RunAt:   time.Now(),
		Payload: job.Payload,
	}

	buffer, err := json.Marshal(work)
	if err != nil {
		return err
	}
	conn.Send("SET", fmt.Sprintf("%sworker:%s", workerSettings.Namespace, w), buffer)
	logger.Debugf("Processing %s since %s [%v]", work.Queue, work.RunAt, work.Payload.Class)

	return conn.Flush()
}

func (w *worker) fail(conn *RedisConn, job *Job, err error) error {
	var backtrace []string
	switch typedError := err.(type) {
	case *WorkerError:
		backtrace = typedError.Backtrace
	default:
		backtrace = []string{}
	}
	failure := &failure{
		FailedAt:  time.Now(),
		Payload:   job.Payload,
		Exception: "Error",
		Error:     err.Error(),
		Backtrace: backtrace,
		Worker:    w,
		Queue:     job.Queue,
	}
	buffer, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	conn.Send("RPUSH", fmt.Sprintf("%sfailed", workerSettings.Namespace), buffer)

	return w.process.fail(conn)
}

func (w *worker) succeed(conn *RedisConn, job *Job) error {
	conn.Send("INCR", fmt.Sprintf("%sstat:processed", workerSettings.Namespace))
	conn.Send("INCR", fmt.Sprintf("%sstat:processed:%s", workerSettings.Namespace, w))

	return nil
}

func (w *worker) finish(conn *RedisConn, job *Job, err error) error {
	if err != nil {
		w.fail(conn, job, err)
	} else {
		w.succeed(conn, job)
	}
	conn.Send("DEL", fmt.Sprintf("%sworker:%s", workerSettings.Namespace, w.process.String()))
	return conn.Flush()
}

func (w *worker) work(jobs <-chan *Job, monitor *sync.WaitGroup) {
	conn, err := GetConn()
	if err != nil {
		logger.Criticalf("Error on getting connection in worker %v: %v", w, err)
		return
	} else {
		w.open(conn)
		PutConn(conn)
	}

	monitor.Add(1)

	go func() {
		defer func() {
			defer monitor.Done()

			conn, err := GetConn()
			if err != nil {
				logger.Criticalf("Error on getting connection in worker %v: %v", w, err)
				return
			} else {
				w.close(conn)
				PutConn(conn)
			}
		}()
		for job := range jobs {
			if workerFunc, ok := workers[job.Payload.Class]; ok {
				w.run(job, workerFunc)

				logger.Debugf("done: (Job{%s} | %s | %v)", job.Queue, job.Payload.Class, job.Payload.Args)
			} else {
				errorLog := fmt.Sprintf("No worker for %s in queue %s with args %v", job.Payload.Class, job.Queue, job.Payload.Args)
				logger.Critical(errorLog)

				conn, err := GetConn()
				if err != nil {
					logger.Criticalf("Error on getting connection in worker %v: %v", w, err)
					return
				} else {
					w.finish(conn, job, errors.New(errorLog))
					PutConn(conn)
				}
			}
		}
	}()
}

func (w *worker) run(job *Job, workerFunc workerFunc) {
	var err error
	defer func() {
		conn, errCon := GetConn()
		if errCon != nil {
			logger.Criticalf("Error on getting connection in worker on finish %v: %v", w, errCon)
			return
		} else {
			w.finish(conn, job, err)
			PutConn(conn)
		}
	}()
	var stackTrace []string
	defer func() {
		if r := recover(); r != nil {
			stackTrace = strings.Split(string(debug.Stack()), "\n")
			err = NewWorkerError(fmt.Sprint(r), stackTrace)
		}
	}()

	conn, err := GetConn()
	if err != nil {
		logger.Criticalf("Error on getting connection in worker on start %v: %v", w, err)
		return
	} else {
		w.start(conn, job)
		PutConn(conn)
	}
	err = workerFunc(job.Queue, job.Payload.Args...)
}

type WorkerError struct {
	message   string
	Backtrace []string
}

func NewWorkerError(message string, backtrace []string) *WorkerError {
	return &WorkerError{message: message, Backtrace: backtrace}
}

func (workerError *WorkerError) Error() string {
	return workerError.message
}
