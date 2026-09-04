package models

import (
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// 所有列都显式声明类型，与 docker/migratesql/migrate.sql 保持一致，理由同 AILLMConfig。
type AICronTaskLog struct {
	Id        int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	TaskId    int64  `json:"task_id" gorm:"type:bigint;not null;default:0"`
	ChatId    string `json:"chat_id" gorm:"type:varchar(64);not null;default:''"`
	SeqId     int64  `json:"seq_id" gorm:"type:bigint;not null;default:0"`
	Status    string `json:"status" gorm:"type:varchar(32);not null;default:'running'"` // running | success | failed
	StartedAt int64  `json:"started_at" gorm:"type:bigint;not null;default:0"`
	EndedAt   int64  `json:"ended_at" gorm:"type:bigint;not null;default:0"`
	Error     string `json:"error" gorm:"type:text"`

	// ChatDeleted 由 logs 接口填充：执行会话被用户删除后，前端据此隐藏
	// 「查看结果」入口（非数据库列）。
	ChatDeleted bool `json:"chat_deleted,omitempty" gorm:"-"`
}

func (l *AICronTaskLog) TableName() string {
	return "ai_cron_task_log"
}

func AICronTaskLogAdd(c *ctx.Context, log *AICronTaskLog) error {
	return Insert(c, log)
}

// AICronTaskLogGetsByTask returns one page of execution logs (id desc) plus
// the total count for the task, following the paginated model function
// convention (see DingtalkGroupsGetByClientIDPage).
func AICronTaskLogGetsByTask(c *ctx.Context, taskId int64, p, limit int) ([]*AICronTaskLog, int64, error) {
	lst := make([]*AICronTaskLog, 0)
	session := DB(c).Where("task_id = ?", taskId)

	var total int64
	if err := session.Model(&AICronTaskLog{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if p <= 0 {
		p = 1
	}
	if limit <= 0 {
		limit = 20
	}

	err := session.Order("id desc").Offset((p - 1) * limit).Limit(limit).Find(&lst).Error
	return lst, total, err
}

func AICronTaskLogUpdateStatus(c *ctx.Context, id int64, status string, endedAt int64, errMsg string) error {
	return DB(c).Model(&AICronTaskLog{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":   status,
		"ended_at": endedAt,
		"error":    errMsg,
	}).Error
}

// AICronTaskLogUpdateSeqId persists the assistant message seq after a
// successful dispatch. Without it the log row has SeqId == 0 and the
// self-heal on the logs endpoint can never observe the message's terminal
// state, leaving the row stuck in "running" forever.
func AICronTaskLogUpdateSeqId(c *ctx.Context, id, seqId int64) error {
	return DB(c).Model(&AICronTaskLog{}).Where("id = ?", id).UpdateColumn("seq_id", seqId).Error
}

// AICronTaskLogDelByTask cleans up execution history together with its task.
func AICronTaskLogDelByTask(c *ctx.Context, taskId int64) error {
	return DB(c).Where("task_id = ?", taskId).Delete(&AICronTaskLog{}).Error
}
