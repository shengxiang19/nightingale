package router

import (
	"errors"
	"net/http"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ginx"
	"github.com/toolkits/pkg/logger"

	"github.com/gin-gonic/gin"
)

// aiCronTaskCanEdit enforces the creator-or-admin edit policy shared by
// put/delete/enable/disable/run. Add has no ownership gate — any user holding
// the /ai-config/cron-tasks permission may create tasks.
func (rt *Router) aiCronTaskCanEdit(me *models.User, task *models.AICronTask) bool {
	return task.CreatedBy == me.Username || me.IsAdmin()
}

func (rt *Router) aiCronTaskGets(c *gin.Context) {
	lst, err := models.AICronTaskGets(rt.Ctx, "")
	ginx.Dangerous(err)
	ginx.NewRender(c).Data(lst, nil)
}

func (rt *Router) aiCronTaskGet(c *gin.Context) {
	id := ginx.UrlParamInt64(c, "id")
	obj, err := models.AICronTaskGetById(rt.Ctx, id)
	ginx.Dangerous(err)
	if obj == nil {
		ginx.Bomb(http.StatusNotFound, "ai cron task not found")
	}
	ginx.NewRender(c).Data(obj, nil)
}

func (rt *Router) aiCronTaskAdd(c *gin.Context) {
	var obj models.AICronTask
	ginx.BindJSON(c, &obj)
	obj.Id = 0
	ginx.Dangerous(obj.Verify())

	// Create() stamps CreatedAt/By and UpdatedAt/By from the username.
	me := c.MustGet("user").(*models.User)
	ginx.Dangerous(obj.Create(rt.Ctx, me.Username))

	if obj.Enabled {
		rt.aiCronOnTaskSaved(&obj)
	}
	ginx.NewRender(c).Data(obj.Id, nil)
}

func (rt *Router) aiCronTaskPut(c *gin.Context) {
	id := ginx.UrlParamInt64(c, "id")
	obj, err := models.AICronTaskGetById(rt.Ctx, id)
	ginx.Dangerous(err)
	if obj == nil {
		ginx.Bomb(http.StatusNotFound, "ai cron task not found")
	}

	me := c.MustGet("user").(*models.User)
	if !rt.aiCronTaskCanEdit(me, obj) {
		ginx.Bomb(http.StatusForbidden, "forbidden")
	}

	var ref models.AICronTask
	ginx.BindJSON(c, &ref)
	ginx.Dangerous(ref.Verify())

	ginx.Dangerous(obj.Update(rt.Ctx, me.Username, ref))

	// Reload the effective row (Update() may normalize fields) and re-sync the
	// scheduler — an update can flip Enabled or change the expression.
	fresh, err := models.AICronTaskGetById(rt.Ctx, id)
	ginx.Dangerous(err)
	if fresh != nil {
		rt.aiCronOnTaskSaved(fresh)
	}
	ginx.NewRender(c).Message(nil)
}

func (rt *Router) aiCronTaskDel(c *gin.Context) {
	id := ginx.UrlParamInt64(c, "id")
	obj, err := models.AICronTaskGetById(rt.Ctx, id)
	ginx.Dangerous(err)
	if obj == nil {
		ginx.Bomb(http.StatusNotFound, "ai cron task not found")
	}

	me := c.MustGet("user").(*models.User)
	if !rt.aiCronTaskCanEdit(me, obj) {
		ginx.Bomb(http.StatusForbidden, "forbidden")
	}

	rt.getAICron().Remove(id)
	ginx.Dangerous(obj.Delete(rt.Ctx))
	ginx.Dangerous(models.AICronTaskLogDelByTask(rt.Ctx, id))
	ginx.NewRender(c).Message(nil)
}

func (rt *Router) aiCronTaskEnable(c *gin.Context) {
	rt.aiCronTaskSetEnabled(c, true)
}

func (rt *Router) aiCronTaskDisable(c *gin.Context) {
	rt.aiCronTaskSetEnabled(c, false)
}

func (rt *Router) aiCronTaskSetEnabled(c *gin.Context, enabled bool) {
	id := ginx.UrlParamInt64(c, "id")
	obj, err := models.AICronTaskGetById(rt.Ctx, id)
	ginx.Dangerous(err)
	if obj == nil {
		ginx.Bomb(http.StatusNotFound, "ai cron task not found")
	}

	me := c.MustGet("user").(*models.User)
	if !rt.aiCronTaskCanEdit(me, obj) {
		ginx.Bomb(http.StatusForbidden, "forbidden")
	}

	obj.Enabled = enabled
	ginx.Dangerous(obj.SetEnabled(rt.Ctx, me.Username, enabled))
	rt.aiCronOnTaskSaved(obj)
	ginx.NewRender(c).Message(nil)
}

func (rt *Router) aiCronTaskRun(c *gin.Context) {
	id := ginx.UrlParamInt64(c, "id")
	obj, err := models.AICronTaskGetById(rt.Ctx, id)
	ginx.Dangerous(err)
	if obj == nil {
		ginx.Bomb(http.StatusNotFound, "ai cron task not found")
	}

	me := c.MustGet("user").(*models.User)
	if !rt.aiCronTaskCanEdit(me, obj) {
		ginx.Bomb(http.StatusForbidden, "forbidden")
	}

	chatId, seqId, logId, err := rt.runAICronTask(obj)
	if err != nil {
		if errors.Is(err, ErrAICronTaskBusy) {
			ginx.Bomb(http.StatusConflict, "%s", err.Error())
			return
		}
		ginx.Dangerous(err)
		return
	}
	ginx.NewRender(c).Data(gin.H{
		"chat_id": chatId,
		"seq_id":  seqId,
		"log_id":  logId,
	}, nil)
}

func (rt *Router) aiCronTaskLogs(c *gin.Context) {
	id := ginx.UrlParamInt64(c, "id")
	obj, err := models.AICronTaskGetById(rt.Ctx, id)
	ginx.Dangerous(err)
	if obj == nil {
		ginx.Bomb(http.StatusNotFound, "ai cron task not found")
	}

	p := ginx.QueryInt(c, "p", 1)
	limit := ginx.QueryInt(c, "limit", 20)

	lst, total, err := models.AICronTaskLogGetsByTask(rt.Ctx, id, p, limit)
	ginx.Dangerous(err)
	rt.aiCronHealRunningLogs(lst)

	// Flag executions whose result chat was deleted by the user, so the
	// frontend can hide the 「查看结果」 entry instead of erroring on click.
	chatIDs := make([]string, 0, len(lst))
	for _, l := range lst {
		if l.ChatId != "" {
			chatIDs = append(chatIDs, l.ChatId)
		}
	}
	if exist, eerr := models.AssistantChatExistIDs(rt.Ctx, chatIDs); eerr != nil {
		logger.Warningf("[AICron] check chat existence task=%d: %v", id, eerr)
	} else {
		for _, l := range lst {
			if l.ChatId != "" && !exist[l.ChatId] {
				l.ChatDeleted = true
			}
		}
	}

	ginx.NewRender(c).Data(gin.H{
		"list":  lst,
		"total": total,
	}, nil)
}

// aiCronHealRunningLogs self-heals rows stuck in "running": the agent finishes
// asynchronously without reporting back to the cron log, so the authoritative
// terminal state is read from the assistant message and written back on the
// next logs read. Rows whose message is still in flight (or gone) stay as-is.
func (rt *Router) aiCronHealRunningLogs(lst []*models.AICronTaskLog) {
	now := time.Now().Unix()
	for _, l := range lst {
		if l.Status != "running" {
			continue
		}
		// Rows dispatched before seq persistence carry SeqId == 0 and can never
		// be healed from the message state. A chat agent run cannot legitimately
		// stay in flight for hours (llm timeouts cap at minutes), so expire them
		// instead of leaving them stuck in "running" forever.
		if l.SeqId <= 0 {
			if l.StartedAt > 0 && now-l.StartedAt > 3600 {
				if uerr := models.AICronTaskLogUpdateStatus(rt.Ctx, l.Id, "failed", now, "dispatch result not recorded (stale running row)"); uerr != nil {
					logger.Warningf("[AICron] expire stale log %d: %v", l.Id, uerr)
					continue
				}
				l.Status = "failed"
				l.EndedAt = now
				l.Error = "dispatch result not recorded (stale running row)"
			}
			continue
		}
		msg, err := models.AssistantMessageGet(rt.Ctx, l.ChatId, l.SeqId)
		if err != nil || msg == nil || !msg.IsFinish {
			continue
		}
		status, errMsg := "success", ""
		if msg.ErrCode != 0 {
			status = "failed"
			errMsg = msg.ErrMsg
		}
		if uerr := models.AICronTaskLogUpdateStatus(rt.Ctx, l.Id, status, now, errMsg); uerr != nil {
			logger.Warningf("[AICron] heal log %d: %v", l.Id, uerr)
			continue
		}
		l.Status = status
		l.EndedAt = now
		l.Error = errMsg
	}
}

// aiCronOnTaskSaved re-syncs the in-memory cron registration with the persisted
// task state and refreshes next_run_at (0 for disabled tasks). Called after
// add/update/enable/disable; there is no periodic reload loop.
func (rt *Router) aiCronOnTaskSaved(task *models.AICronTask) {
	rt.getAICron().Reschedule(task)

	next := int64(0)
	if task.Enabled {
		n, err := aiCronNextRunAt(task.CronExpr, time.Now())
		if err != nil {
			logger.Warningf("[AICron] compute next run task=%d expr=%q: %v", task.Id, task.CronExpr, err)
		} else {
			next = n
		}
	}
	if err := models.AICronTaskSetNextRunAt(rt.Ctx, task.Id, next); err != nil {
		logger.Warningf("[AICron] persist next_run_at task=%d: %v", task.Id, err)
	}
	task.NextRunAt = next
}
