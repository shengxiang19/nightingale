package models

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/toolkits/pkg/logger"
	"gorm.io/gorm"
)

// AIAgent.UseCase 的已注册取值。UseCase 本身是开放 varchar（运维可自定义），但每个
// 取值都必须在 center/router 的 turnResourceChains 里注册一条资源链（值守模式 +
// LLM/技能优先级链），否则该 use_case 的轮次会回落 chat 链并记一条 warning。
const (
	// UseCaseChat 交互式对话：用户在场，模型可反问，工具层允许人在环确认。
	UseCaseChat = "chat"
	// UseCaseCronTask 无人值守的定时任务执行轮：固定 action、汇报式 prompt、禁止反问。
	// 工具面与 chat 相同（写工具靠自身缺参门/提案确认门兜底，中断由人接管）。
	UseCaseCronTask = "cron_task"
)

// 所有列都显式声明类型，与 docker/migratesql/migrate.sql 保持一致，理由同 AILLMConfig。
type AIAgent struct {
	Id           int64   `json:"id" gorm:"primaryKey;autoIncrement"`
	Name         string  `json:"name" gorm:"type:varchar(255);not null;default:''"`
	Description  string  `json:"description" gorm:"type:text"`
	UseCase      string  `json:"use_case" gorm:"type:varchar(64);not null;default:''"`
	LLMConfigId  int64   `json:"llm_config_id" gorm:"type:bigint;not null;default:0"`
	SkillIds     []int64 `json:"skill_ids,omitempty" gorm:"serializer:json;type:text"`
	MCPServerIds []int64 `json:"mcp_server_ids,omitempty" gorm:"serializer:json;type:text"`
	Enabled      bool    `json:"enabled" gorm:"type:boolean;not null;default:false"`
	CreatedAt    int64   `json:"created_at" gorm:"type:bigint;not null;default:0"`
	CreatedBy    string  `json:"created_by" gorm:"type:varchar(64);not null;default:''"`
	UpdatedAt    int64   `json:"updated_at" gorm:"type:bigint;not null;default:0"`
	UpdatedBy    string  `json:"updated_by" gorm:"type:varchar(64);not null;default:''"`

	LLMConfigName string `json:"llm_config_name" gorm:"-"`
}

func (a *AIAgent) TableName() string {
	return "ai_agent"
}

func (a *AIAgent) Verify() error {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	if a.LLMConfigId <= 0 {
		return fmt.Errorf("llm_config_id is required")
	}
	return nil
}

func AIAgentGets(c *ctx.Context) ([]*AIAgent, error) {
	var lst []*AIAgent
	err := DB(c).Order("id").Find(&lst).Error
	return lst, err
}

func AIAgentGet(c *ctx.Context, where string, args ...interface{}) (*AIAgent, error) {
	var obj AIAgent
	err := DB(c).Where(where, args...).First(&obj).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &obj, nil
}

func AIAgentGetById(c *ctx.Context, id int64) (*AIAgent, error) {
	return AIAgentGet(c, "id = ?", id)
}

func AIAgentGetByName(c *ctx.Context, name string) (*AIAgent, error) {
	return AIAgentGet(c, "name = ?", name)
}

func (a *AIAgent) Create(c *ctx.Context, username string) error {
	exist, err := AIAgentGetByName(c, a.Name)
	if err != nil {
		return err
	}
	if exist != nil {
		return fmt.Errorf("ai agent name %s already exists", a.Name)
	}

	now := time.Now().Unix()
	a.CreatedAt = now
	a.UpdatedAt = now
	a.CreatedBy = username
	a.UpdatedBy = username
	return Insert(c, a)
}

func (a *AIAgent) Update(c *ctx.Context, username string, data AIAgent) error {
	if data.Name != a.Name {
		exist, err := AIAgentGetByName(c, data.Name)
		if err != nil {
			return err
		}
		if exist != nil {
			return fmt.Errorf("ai agent name %s already exists", data.Name)
		}
	}

	data.UpdatedAt = time.Now().Unix()
	data.UpdatedBy = username

	return DB(c).Model(a).Select("name", "description", "use_case", "llm_config_id",
		"skill_ids", "mcp_server_ids",
		"enabled", "updated_at", "updated_by").Updates(data).Error
}

func (a *AIAgent) Delete(c *ctx.Context) error {
	return DB(c).Where("id = ?", a.Id).Delete(&AIAgent{}).Error
}

func AIAgentGetByUseCase(c *ctx.Context, useCase string) (*AIAgent, error) {
	return AIAgentGet(c, "use_case = ? and enabled = ?", useCase, true)
}

func AIAgentStatistics(c *ctx.Context) (*Statistics, error) {
	return StatisticsGet(c, &AIAgent{})
}

// InitDefaultAIAgent ensures every built-in use_case has an agent, so aichat works
// even when the admin UI no longer exposes agent management. Idempotent:
//   - use_case=chat — seeded only when the whole table is empty (historic
//     behavior; an admin who deletes it while other agents exist is respected).
//   - use_case=cron_task — seeded whenever no agent claims that use_case at all,
//     because every ai_cron_task defaults to it and a missing baseline would fail
//     every scheduled run with 400.
//
// LLMConfigId=0 in both cases — the turn chain resolves it to the default LLM via
// AILLMConfigPickDefault when a run actually happens.
func InitDefaultAIAgent(c *ctx.Context) {
	var count int64
	if err := DB(c).Model(&AIAgent{}).Count(&count).Error; err != nil {
		logger.Warningf("InitDefaultAIAgent count failed: %v", err)
		return
	}
	if count == 0 {
		seedDefaultAIAgent(c, UseCaseChat, "default-chat-agent",
			"auto-created default chat agent; uses the default LLM config at runtime")
	}

	var cronCount int64
	if err := DB(c).Model(&AIAgent{}).Where("use_case = ?", UseCaseCronTask).Count(&cronCount).Error; err != nil {
		logger.Warningf("InitDefaultAIAgent count cron agents failed: %v", err)
		return
	}
	if cronCount == 0 {
		seedDefaultAIAgent(c, UseCaseCronTask, "default-cron-task-agent",
			"auto-created default agent for unattended scheduled tasks (read-only tools); uses the default LLM config at runtime")
	}
}

func seedDefaultAIAgent(c *ctx.Context, useCase, name, description string) {
	now := time.Now().Unix()
	agent := &AIAgent{
		Name:        name,
		Description: description,
		UseCase:     useCase,
		LLMConfigId: 0,
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
		CreatedBy:   "system",
		UpdatedBy:   "system",
	}
	if err := Insert(c, agent); err != nil {
		logger.Warningf("InitDefaultAIAgent insert use_case=%s failed: %v", useCase, err)
	}
}
