package models

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// 所有列都显式声明类型，与 docker/migratesql/migrate.sql 保持一致，理由同 AILLMConfig。
type AICronTask struct {
	Id          int64   `json:"id" gorm:"primaryKey;autoIncrement"`
	Name        string  `json:"name" gorm:"type:varchar(255);not null;default:''"`
	Description string  `json:"description" gorm:"type:text"`
	CronExpr    string  `json:"cron_expr" gorm:"type:varchar(64);not null;default:''"`
	UseCase     string  `json:"use_case" gorm:"type:varchar(64);not null;default:'chat'"`
	LLMConfigId int64   `json:"llm_config_id" gorm:"type:bigint;not null;default:0"` // 0 = use default LLM
	SkillIds    []int64 `json:"skill_ids,omitempty" gorm:"serializer:json;type:text"`
	Content     string  `json:"content" gorm:"type:text"` // the prompt/instruction sent to the agent
	Enabled     bool    `json:"enabled" gorm:"type:boolean;not null;default:false"`
	LastRunAt   int64   `json:"last_run_at" gorm:"type:bigint;not null;default:0"`
	NextRunAt   int64   `json:"next_run_at" gorm:"type:bigint;not null;default:0"`
	LastStatus  string  `json:"last_status" gorm:"type:varchar(32);not null;default:''"` // success | failed | running
	CreatedAt   int64   `json:"created_at" gorm:"type:bigint;not null;default:0"`
	CreatedBy   string  `json:"created_by" gorm:"type:varchar(64);not null;default:''"`
	UpdatedAt   int64   `json:"updated_at" gorm:"type:bigint;not null;default:0"`
	UpdatedBy   string  `json:"updated_by" gorm:"type:varchar(64);not null;default:''"`

	LLMConfigName string `json:"llm_config_name" gorm:"-"`
}

func (a *AICronTask) TableName() string {
	return "ai_cron_task"
}

func (a *AICronTask) Verify() error {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}

	a.CronExpr = strings.TrimSpace(a.CronExpr)
	if a.CronExpr == "" {
		return fmt.Errorf("cron_expr is required")
	}
	// The UI uses standard 5-field expressions like "0 */1 * * *"; the
	// Descriptor option additionally accepts @hourly/@daily/... forms.
	if _, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(a.CronExpr); err != nil {
		return fmt.Errorf("invalid cron expression %q: %v", a.CronExpr, err)
	}

	a.Content = strings.TrimSpace(a.Content)
	if a.Content == "" {
		return fmt.Errorf("content is required")
	}

	// LLMConfigId == 0 means the default LLM config is used at runtime.
	return nil
}

func AICronTaskGets(c *ctx.Context, where string, args ...interface{}) ([]*AICronTask, error) {
	var lst []*AICronTask
	session := DB(c)
	if where != "" {
		session = session.Where(where, args...)
	}
	err := session.Order("id desc").Find(&lst).Error
	if err != nil {
		return nil, err
	}
	if err := fillAICronTaskLLMConfigNames(c, lst); err != nil {
		return nil, err
	}
	return lst, nil
}

// fillAICronTaskLLMConfigNames batch-fills LLMConfigName for the given tasks.
// LLMConfigId == 0 means the default LLM config is used at runtime; the name
// stays empty, same semantics as AIAgent.
func fillAICronTaskLLMConfigNames(c *ctx.Context, lst []*AICronTask) error {
	ids := make([]int64, 0, len(lst))
	for _, item := range lst {
		if item.LLMConfigId > 0 {
			ids = append(ids, item.LLMConfigId)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	configs, err := AILLMConfigGetByIds(c, ids)
	if err != nil {
		return err
	}

	configMap := make(map[int64]string, len(configs))
	for _, cfg := range configs {
		configMap[cfg.Id] = cfg.Name
	}

	for _, item := range lst {
		if item.LLMConfigId > 0 {
			item.LLMConfigName = configMap[item.LLMConfigId]
		}
	}
	return nil
}

func AICronTaskGet(c *ctx.Context, where string, args ...interface{}) (*AICronTask, error) {
	var obj AICronTask
	err := DB(c).Where(where, args...).First(&obj).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if err := fillAICronTaskLLMConfigNames(c, []*AICronTask{&obj}); err != nil {
		return nil, err
	}
	return &obj, nil
}

func AICronTaskGetById(c *ctx.Context, id int64) (*AICronTask, error) {
	return AICronTaskGet(c, "id = ?", id)
}

// AICronTaskExistsByName reports whether a task with the given name already
// exists. Uses Count instead of First: a missing row would otherwise surface
// as a gorm "record not found" ERROR log on every successful rename/create.
func AICronTaskExistsByName(c *ctx.Context, name string) (bool, error) {
	var n int64
	err := DB(c).Model(&AICronTask{}).Where("name = ?", name).Count(&n).Error
	return n > 0, err
}

func (a *AICronTask) Create(c *ctx.Context, username string) error {
	exist, err := AICronTaskExistsByName(c, a.Name)
	if err != nil {
		return err
	}
	if exist {
		return fmt.Errorf("ai cron task name %s already exists", a.Name)
	}

	now := time.Now().Unix()
	a.CreatedAt = now
	a.UpdatedAt = now
	a.CreatedBy = username
	a.UpdatedBy = username
	return Insert(c, a)
}

func (a *AICronTask) Update(c *ctx.Context, username string, data AICronTask) error {
	if data.Name != a.Name {
		exist, err := AICronTaskExistsByName(c, data.Name)
		if err != nil {
			return err
		}
		if exist {
			return fmt.Errorf("ai cron task name %s already exists", data.Name)
		}
	}

	data.UpdatedAt = time.Now().Unix()
	data.UpdatedBy = username

	// last_run_at/next_run_at/last_status are runtime-managed by the
	// scheduler and deliberately not user-editable here.
	return DB(c).Model(a).Select("name", "description", "cron_expr", "use_case",
		"llm_config_id", "skill_ids", "content", "enabled",
		"updated_at", "updated_by").Updates(data).Error
}

func (a *AICronTask) SetEnabled(c *ctx.Context, username string, enabled bool) error {
	return DB(c).Model(a).Updates(map[string]interface{}{
		"enabled":    enabled,
		"updated_at": time.Now().Unix(),
		"updated_by": username,
	}).Error
}

func (a *AICronTask) Delete(c *ctx.Context) error {
	return DB(c).Where("id = ?", a.Id).Delete(&AICronTask{}).Error
}

// Runtime-managed columns (last_run_at/next_run_at/last_status) are written
// only by the scheduler via the helpers below. UpdateColumn bypasses gorm's
// updated_at auto-touch, so background runs don't masquerade as edits.

func AICronTaskUpdateRuntime(c *ctx.Context, id int64, lastRunAt, nextRunAt int64, lastStatus string) error {
	return DB(c).Model(&AICronTask{}).Where("id = ?", id).UpdateColumns(map[string]interface{}{
		"last_run_at": lastRunAt,
		"next_run_at": nextRunAt,
		"last_status": lastStatus,
	}).Error
}

func AICronTaskSetNextRunAt(c *ctx.Context, id int64, nextRunAt int64) error {
	return DB(c).Model(&AICronTask{}).Where("id = ?", id).UpdateColumn("next_run_at", nextRunAt).Error
}
