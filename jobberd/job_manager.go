package main

import (
    "time"
    "log"
    "log/syslog"
    "fmt"
    "strings"
    "sort"
    "text/tabwriter"
    "bytes"
)

var Logger *log.Logger = nil
var ErrLogger *log.Logger = nil

type JobberError struct {
    What  string
    Cause error
}

func (e *JobberError) Error() string {
    if e.Cause == nil {
        return e.What
    } else {
        return e.What + ":" + e.Cause.Error()
    }
}

type RunLogEntry struct {
    Job        *Job
    Time       time.Time
    Succeeded  bool
    Result     JobStatus
}

/* For sorting RunLogEntries: */
type runLogEntrySorter struct {
    entries []RunLogEntry
}

/* For sorting RunLogEntries: */
func (s *runLogEntrySorter) Len() int {
    return len(s.entries)
}

/* For sorting RunLogEntries: */
func (s *runLogEntrySorter) Swap(i, j int) {
    s.entries[i], s.entries[j] = s.entries[j], s.entries[i]
}

/* For sorting RunLogEntries: */
func (s *runLogEntrySorter) Less(i, j int) bool {
    return s.entries[i].Time.After(s.entries[j].Time)
}

type JobManager struct {
    jobs                  []*Job
    loadedJobs            bool
    runLog                []RunLogEntry
    cmdChan               chan ICmd
    mainThreadCtx         *JobberContext
    mainThreadCtl         JobberCtl
    jobRunner             *JobRunnerThread
    Shell                 string
}

func NewJobManager() (*JobManager, error) {
    var err error
    jm := JobManager{Shell: "/bin/sh"}
    Logger, err = syslog.NewLogger(syslog.LOG_NOTICE | syslog.LOG_CRON, 0)
    if err != nil {
        return nil, &JobberError{What: "Couldn't make Syslog logger.", Cause: err}
    }
    ErrLogger, err = syslog.NewLogger(syslog.LOG_ERR | syslog.LOG_CRON, 0)
    if err != nil {
        return nil, &JobberError{What: "Couldn't make Syslog logger.", Cause: err}
    }
    jm.loadedJobs = false
    jm.jobRunner = NewJobRunnerThread()
    return &jm, nil
}

func (m *JobManager) jobsForUser(username string) []*Job {
    jobs := make([]*Job, 0)
    for _, job := range m.jobs {
        if username == job.User {
            jobs = append(jobs, job)
        }
    }
    return jobs
}

func (m *JobManager) runLogEntriesForUser(username string) []RunLogEntry {
    entries := make([]RunLogEntry, 0)
    for _, entry := range m.runLog {
        if username == entry.Job.User {
            entries = append(entries, entry)
        }
    }
    return entries
}

func (m *JobManager) Launch() (chan<- ICmd, error) {
    if m.mainThreadCtx != nil {
        return nil, &JobberError{"Already launched.", nil}
    }
    
    Logger.Println("Launching.")
    if !m.loadedJobs {
        _, err := m.LoadAllJobs()
        if err != nil {
            ErrLogger.Printf("Failed to load jobs: %v.\n", err)
            return nil, err
        }
    }
    
    // make main thread
    m.cmdChan = make(chan ICmd)
    m.runMainThread()
    return m.cmdChan, nil
}

func (m *JobManager) Cancel() {
    if m.mainThreadCtl.Cancel != nil {
        Logger.Printf("JobManager canceling\n")
        m.mainThreadCtl.Cancel()
    }
}

func (m *JobManager) Wait() {
    if m.mainThreadCtl.Wait != nil {
        m.mainThreadCtl.Wait()
    }
}

func (m *JobManager) handleRunRec(rec *RunRec) {
    if len(rec.Stdout) > 0 {
        Logger.Println(rec.Stdout)
    }
    if len(rec.Stderr) > 0 {
        ErrLogger.Println(rec.Stderr)
    }
    if rec.Err != nil {
        ErrLogger.Panicln(rec.Err)
    }
    
    m.runLog = append(m.runLog, RunLogEntry{rec.Job, rec.RunTime, rec.Succeeded, rec.NewStatus})
    
    /* NOTE: error-handler was already applied by the job, if necessary. */
    
    if (!rec.Succeeded && rec.Job.NotifyOnError) ||
        (rec.Job.NotifyOnFailure && rec.NewStatus == JobFailed) {
        // notify user
        headers := fmt.Sprintf("To: %v\r\nFrom: %v\r\nSubject: \"%v\" failed.", rec.Job.User, rec.Job.User, rec.Job.Name)
        bod := rec.Describe()
        msg := fmt.Sprintf("%s\r\n\r\n%s.\r\n", headers, bod)
        sendmailCmd := fmt.Sprintf("sendmail %v", rec.Job.User)
        sudoResult, err := sudo(rec.Job.User, sendmailCmd, "/bin/sh", &msg)
        if err != nil {
            ErrLogger.Println("Failed to send mail: %v", err)
        } else if !sudoResult.Succeeded {
            ErrLogger.Println("Failed to send mail: %v", sudoResult.Stderr)
        }
    }
}

func (m *JobManager) runMainThread() {
    m.mainThreadCtx, m.mainThreadCtl = NewJobberContext(BackgroundJobberContext())
    Logger.Printf("Main thread context: %v\n", m.mainThreadCtx.Name)
    
    go func() {
        /*
         All modifications to the job manager's state occur here.
        */
    
        // start job-runner thread
        m.jobRunner.Start(m.jobs, m.Shell, m.mainThreadCtx)
    
        Loop: for {
            select {
            case <-m.mainThreadCtx.Done():
                Logger.Printf("Main thread got 'stop'\n")
                break Loop
                
            case rec, ok := <-m.jobRunner.RunRecChan():
                if ok {
                    m.handleRunRec(rec)
                } else {
                    ErrLogger.Printf("Job-runner thread ended prematurely.\n")
                    break Loop
                }
    
            case cmd, ok := <-m.cmdChan:
                if ok {
                    //fmt.Printf("JobManager: processing cmd.\n")
                    shouldStop := m.doCmd(cmd)
                    if shouldStop {
                        break Loop
                    }
                } else {
                    ErrLogger.Printf("Command channel was closed.\n")
                    break Loop
                }
            }
        }
        
        // cancel main thread
        m.mainThreadCtl.Cancel()
        
        // consume all run-records
        for rec := range m.jobRunner.RunRecChan() {
            m.handleRunRec(rec)
        }
        
        // finish up (and wait for job-runner thread to finish)
        m.mainThreadCtx.Finish()
        
        Logger.Printf("Main Thread done.\n")
    }()
}

func (m *JobManager) doCmd(cmd ICmd) bool {  // runs in main thread
    
    /*
    Security:
    
    It is jobberd's responsibility to enforce the security policy.
    
    It does so by assuming that cmd.RequestingUser() is truly the name
    of the requesting user.
    */
    
    switch cmd.(type) {
    case *ReloadCmd:
        /* Policy: Only root can reload other users' jobfiles. */
        
        // load jobs
        var err error
        var amt int
        if cmd.(*ReloadCmd).ForAllUsers {
            if cmd.RequestingUser() != "root" {
                cmd.RespChan() <- &ErrorCmdResp{&JobberError{What: "You must be root."}}
                break
            }
            
            Logger.Printf("Reloading jobs for all users.\n")
            amt, err = m.ReloadAllJobs()
        } else {
            Logger.Printf("Reloading jobs for %v.\n", cmd.RequestingUser())
            amt, err = m.ReloadJobsForUser(cmd.RequestingUser())
        }
        
        // send response
        if err != nil {
            ErrLogger.Printf("Failed to load jobs: %v.\n", err)
            cmd.RespChan() <- &ErrorCmdResp{err}
        } else {
            cmd.RespChan() <- &SuccessCmdResp{fmt.Sprintf("Loaded %v jobs.", amt)}
        }
        
        return false
    
    case *ListJobsCmd:
        /* Policy: Only root can list other users' jobs. */
        
        // get jobs
        var jobs []*Job
        if cmd.(*ListJobsCmd).ForAllUsers {
            if cmd.RequestingUser() != "root" {
                cmd.RespChan() <- &ErrorCmdResp{&JobberError{What: "You must be root."}}
                break
            }
            
            jobs = m.jobs
        } else {
            jobs = m.jobsForUser(cmd.RequestingUser()) 
        }
        
        // make response
        var buffer bytes.Buffer
        var writer *tabwriter.Writer = tabwriter.NewWriter(&buffer, 5, 0, 2, ' ', 0)
        fmt.Fprintf(writer, "NAME\tSTATUS\tSEC\tMIN\tHOUR\tMDAY\tMONTH\tWDAY\tCOMMAND\tNOTIFY ON ERROR\tNOTIFY ON FAILURE\tERROR HANDLER\t\n")
        strs := make([]string, 0, len(m.jobs))
        for _, j := range jobs {
            cmdStrMaxLen := 40
            cmdStr := j.Cmd
            if len(cmdStr) > cmdStrMaxLen {
                cmdStr = cmdStr[0:cmdStrMaxLen-3] + "..."
            }
            s := fmt.Sprintf("%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t\"%v\"\t%v\t%v\t%v\t",
                               j.Name,
                               j.Status,
                               j.Sec,
                               j.Min,
                               j.Hour,
                               j.Mday,
                               j.Mon,
                               j.Wday,
                               cmdStr,
                               j.NotifyOnError,
                               j.NotifyOnFailure,
                               j.ErrorHandler)
            strs = append(strs, s)
        }
        fmt.Fprintf(writer, "%v", strings.Join(strs, "\n"))
        writer.Flush()
        
        // send response
        cmd.RespChan() <- &SuccessCmdResp{buffer.String()}
        
        return false
    
    case *ListHistoryCmd:
        /* Policy: Only root can see the histories of other users' jobs. */
        
        // get log entries
        var entries []RunLogEntry
        if cmd.(*ListHistoryCmd).ForAllUsers {
            if cmd.RequestingUser() != "root" {
                cmd.RespChan() <- &ErrorCmdResp{&JobberError{What: "You must be root."}}
                break
            }
            
            entries = m.runLog
        } else {
            entries = m.runLogEntriesForUser(cmd.RequestingUser()) 
        }
        sort.Sort(&runLogEntrySorter{entries})
        
        // make response
        var buffer bytes.Buffer
        var writer *tabwriter.Writer = tabwriter.NewWriter(&buffer, 5, 0, 2, ' ', 0)
        fmt.Fprintf(writer, "TIME\tJOB\tUSER\tSUCCEEDED\tRESULT\t\n")
        strs := make([]string, 0, len(m.jobs))
        for _, e := range entries {
            s := fmt.Sprintf("%v\t%v\t%v\t%v\t%v\t", e.Time, e.Job.Name, e.Job.User, e.Succeeded, e.Result)
            strs = append(strs, s)
        }
        fmt.Fprintf(writer, "%v", strings.Join(strs, "\n"))
        writer.Flush()
    
        // send response
        cmd.RespChan() <- &SuccessCmdResp{buffer.String()}
        
        return false
    
    case *StopCmd:
        /* Policy: Only root can stop jobberd. */
        
        if cmd.RequestingUser() != "root" {
            cmd.RespChan() <- &ErrorCmdResp{&JobberError{What: "You must be root."}}
            break
        }
        
        Logger.Println("Stopping.")
        return true
    
    case *TestCmd:
        /* Policy: Only root can test other users' jobs. */
        
        var testCmd *TestCmd = cmd.(*TestCmd)
        
        // enfore policy
        if testCmd.jobUser != testCmd.RequestingUser() && testCmd.RequestingUser() != "root" {
            cmd.RespChan() <- &ErrorCmdResp{&JobberError{What: "You must be root."}}
            break
        }
    
        // find job to test
        var job_p *Job
        for _, job := range m.jobsForUser(testCmd.jobUser) {
            if job.Name == testCmd.job {
                job_p = job
                break
            }
        }
        if job_p == nil {
            msg := fmt.Sprintf("No job named \"%v\".", testCmd.job)
            cmd.RespChan() <- &ErrorCmdResp{&JobberError{What: msg}}
            break
        }
        
        // run the job in this thread
        runRec := job_p.Run(nil, m.Shell, true)
        
        // send response
        if runRec.Err != nil {
            cmd.RespChan() <- &ErrorCmdResp{runRec.Err}
            break
        }
        cmd.RespChan() <- &SuccessCmdResp{Details: runRec.Describe()}
        
        return false
    
    default:
        cmd.RespChan() <- &ErrorCmdResp{&JobberError{What: "Unknown command."}}
        return false
    }
    
    return false
}
