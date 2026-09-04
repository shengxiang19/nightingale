package router

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	cron "github.com/robfig/cron/v3"
	"github.com/toolkits/pkg/logger"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// aiCronParser must match models.AICronTask.Verify(): standard 5-field
// expressions plus @hourly/@daily/... descriptor forms.
var aiCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ErrAICronTaskBusy means the task already has a run in flight (or its chat is
// still busy with the previous message). Callers may map it to HTTP 409 via
// errors.Is, mirroring the chat-busy convention of StartAssistantMessage.
var ErrAICronTaskBusy = errors.New("previous run still in progress")

// AICronScheduler keeps in-memory cron registrations for enabled ai_cron_task
// rows. Registration happens at startup and incrementally on add/update/
// enable/disable/delete API calls — no periodic reload. Both maps are guarded
// by mu; the robfig cron instance is internally synchronized.
type AICronScheduler struct {
	mu       sync.Mutex
	cron     *cron.Cron
	entries  map[int64]cron.EntryID
	inflight map[int64]bool // task id → a run is being dispatched, prevents overlapping runs
	ctx      *ctx.Context   // handle for model getters; set at construction, refreshed at startup
	rt       *Router        // for runAICronTask; Router is a stable pointer
}

func newAICronScheduler(ctx *ctx.Context, rt *Router) *AICronScheduler {
	return &AICronScheduler{
		cron:     cron.New(cron.WithParser(aiCronParser)),
		entries:  make(map[int64]cron.EntryID),
		inflight: make(map[int64]bool),
		ctx:      ctx,
		rt:       rt,
	}
}

// getAICron lazily initializes the scheduler so enable/disable/run handlers
// work even in embedders that never start the scheduler loop.
func (rt *Router) getAICron() *AICronScheduler {
	rt.aiCronMu.Lock()
	defer rt.aiCronMu.Unlock()
	if rt.aiCron == nil {
		rt.aiCron = newAICronScheduler(rt.Ctx, rt)
	}
	return rt.aiCron
}

// StartAICronTaskScheduler loads all enabled tasks, registers their cron
// entries and starts ticking. Runs once at center startup (see center.go);
// later task changes are applied incrementally by the CRUD handlers.
func (rt *Router) StartAICronTaskScheduler(ctx *ctx.Context) {
	s := rt.getAICron()
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	var tasks []*models.AICronTask
	var err error
	// A transient DB hiccup at boot must not silently disable every task for
	// the process lifetime, so retry a few times before giving up.
	for i := 0; i < 3; i++ {
		tasks, err = models.AICronTaskGets(ctx, "enabled = ?", true)
		if err == nil {
			break
		}
		logger.Errorf("[AICron] load enabled tasks attempt=%d: %v", i+1, err)
		time.Sleep(time.Second)
	}
	if err != nil {
		logger.Errorf("[AICron] scheduler starts without registrations; tasks will register on API edits: %v", err)
	}

	for _, task := range tasks {
		s.Add(task)
	}
	s.cron.Start()
	logger.Infof("[AICron] scheduler started, %d task(s) registered", len(tasks))
}

// Add registers the task's cron entry. Entries fire in their own goroutine
// (robfig v3 startJob), so the ticker is never blocked by a slow run.
func (s *AICronScheduler) Add(task *models.AICronTask) {
	if task == nil || !task.Enabled {
		return
	}

	taskId := task.Id
	entryId, err := s.cron.AddFunc(task.CronExpr, func() {
		// Reload instead of capturing the registration-time snapshot: edits,
		// disable and delete after registration must be honored, and the
		// persisted ChatId (assigned on first run) must be reused.
		fresh, ferr := models.AICronTaskGetById(s.ctx, taskId)
		if ferr != nil {
			logger.Warningf("[AICron] reload task %d: %v", taskId, ferr)
			return
		}
		if fresh == nil || !fresh.Enabled {
			return
		}
		if _, _, _, rerr := s.rt.runAICronTask(fresh); rerr != nil && !errors.Is(rerr, ErrAICronTaskBusy) {
			logger.Errorf("[AICron] task %d(%s) run failed: %v", fresh.Id, fresh.Name, rerr)
		}
	})
	if err != nil {
		// Verify() rejects the same malformed expressions, so this only
		// happens if the row was edited out-of-band.
		logger.Errorf("[AICron] register task %d expr %q: %v", taskId, task.CronExpr, err)
		return
	}

	s.mu.Lock()
	if old, ok := s.entries[taskId]; ok {
		s.cron.Remove(old)
	}
	s.entries[taskId] = entryId
	s.mu.Unlock()
}

// Remove unregisters the task. No-op when it was never registered.
func (s *AICronScheduler) Remove(taskId int64) {
	s.mu.Lock()
	entryId, ok := s.entries[taskId]
	if ok {
		delete(s.entries, taskId)
	}
	s.mu.Unlock()
	if ok {
		s.cron.Remove(entryId)
	}
}

// Reschedule replaces the task's registration; disabled tasks are removed.
func (s *AICronScheduler) Reschedule(task *models.AICronTask) {
	if task == nil {
		return
	}
	s.Remove(task.Id)
	s.Add(task)
}

// aiCronNextRunAt computes the next fire time of a cron expression after from.
func aiCronNextRunAt(expr string, from time.Time) (int64, error) {
	sched, err := aiCronParser.Parse(expr)
	if err != nil {
		return 0, err
	}
	return sched.Next(from).Unix(), nil
}

// aiCronNewRunChat creates a dedicated assistant chat for ONE execution of the
// task, owned by the task creator. Per-run chats (instead of one accumulating
// per-task chat) let 「查看结果」 point each execution history row to its own
// conversation, and let the sidebar group them under the task as a folder.
// TaskId/TaskName mark the chat so it is excluded from the normal chat history
// and aggregated under the sidebar 「任务」 section; TaskName is denormalized so
// the folder label survives task deletion.
func (rt *Router) aiCronNewRunChat(task *models.AICronTask) (*models.AssistantChat, error) {
	creator, err := models.UserGetByUsername(rt.Ctx, task.CreatedBy)
	if err != nil {
		return nil, err
	}
	if creator == nil {
		return nil, fmt.Errorf("task creator %q not found, cannot create chat", task.CreatedBy)
	}

	chat := models.AssistantChat{
		ChatID: uuid.New().String(),
		Title:  fmt.Sprintf("%s %s", task.Name, time.Now().Format("01-02 15:04")),
		// IsRenamed keeps the title above: without it the first message would
		// overwrite the title with the query content.
		IsRenamed:  true,
		LastUpdate: time.Now().Unix(),
		UserID:     creator.Id,
		IsNew:      true,
		TaskId:     task.Id,
		TaskName:   task.Name,
	}
	if err := models.AssistantChatSet(rt.Ctx, chat); err != nil {
		return nil, fmt.Errorf("create assistant chat: %w", err)
	}
	return &chat, nil
}

// aiCronUpdateRuntime persists the scheduler-managed runtime columns; failures
// are logged, not fatal — they never abort a dispatch that already succeeded.
func (rt *Router) aiCronUpdateRuntime(taskId, lastRunAt, nextRunAt int64, lastStatus string) {
	if err := models.AICronTaskUpdateRuntime(rt.Ctx, taskId, lastRunAt, nextRunAt, lastStatus); err != nil {
		logger.Warningf("[AICron] update runtime fields task=%d: %v", taskId, err)
	}
}

// runAICronTask is the single dispatch path shared by the cron trigger and the
// run-now API. It creates a dedicated chat for this run, records a running log
// row and hands the prompt to the assistant pipeline (which returns immediately
// after spawning the runner goroutine — agent completion is observed via log
// enrichment on the logs endpoint, not here).
func (rt *Router) runAICronTask(task *models.AICronTask) (chatId string, seqId int64, logId int64, err error) {
	s := rt.getAICron()

	s.mu.Lock()
	if s.inflight[task.Id] {
		s.mu.Unlock()
		return "", 0, 0, ErrAICronTaskBusy
	}
	s.inflight[task.Id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, task.Id)
		s.mu.Unlock()
	}()

	chat, err := rt.aiCronNewRunChat(task)
	if err != nil {
		return "", 0, 0, err
	}
	chatId = chat.ChatID

	now := time.Now().Unix()
	log := &models.AICronTaskLog{
		TaskId:    task.Id,
		ChatId:    chatId,
		Status:    "running",
		StartedAt: now,
	}
	if err = models.AICronTaskLogAdd(rt.Ctx, log); err != nil {
		return chatId, 0, 0, err
	}
	logId = log.Id

	result, status, err := rt.StartAssistantMessage(chat.UserID, chat, models.AssistantMessageQuery{Content: task.Content}, "")
	if err != nil {
		// A fresh per-run chat cannot be busy, but treat any dispatch failure
		// (including an unlikely 409 race) uniformly: mark the log failed.
		if uerr := models.AICronTaskLogUpdateStatus(rt.Ctx, logId, "failed", time.Now().Unix(), err.Error()); uerr != nil {
			logger.Warningf("[AICron] mark failed log %d: %v", logId, uerr)
		}
		rt.aiCronUpdateRuntime(task.Id, now, 0, "failed")
		if status == http.StatusConflict {
			return chatId, 0, logId, ErrAICronTaskBusy
		}
		return chatId, 0, logId, err
	}

	seqId = result.SeqID
	// Persist the seq so the log row can self-heal from the assistant message's
	// terminal state on the next logs read (see aiCronHealRunningLogs).
	if uerr := models.AICronTaskLogUpdateSeqId(rt.Ctx, logId, seqId); uerr != nil {
		logger.Warningf("[AICron] persist seq log %d seq %d: %v", logId, seqId, uerr)
	}
	next, nerr := aiCronNextRunAt(task.CronExpr, time.Now())
	if nerr != nil {
		logger.Warningf("[AICron] compute next run task=%d expr=%q: %v", task.Id, task.CronExpr, nerr)
	}
	rt.aiCronUpdateRuntime(task.Id, now, next, "success")
	return chatId, seqId, logId, nil
}
