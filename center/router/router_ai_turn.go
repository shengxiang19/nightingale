package router

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ccfos/nightingale/v6/aiagent"
	"github.com/ccfos/nightingale/v6/aiagent/chat"
	"github.com/ccfos/nightingale/v6/models"

	"github.com/toolkits/pkg/logger"
)

// ==================== 运行入口参数 ====================

// AssistantRunOptions 携带某一次 assistant 运行的外部参数（use_case / llm_config_id /
// skill_ids）。普通 chat 传 nil 表示全部走 chat 默认（chat agent、chat 链）；
// AI 定时任务在入口传入任务自带的取值：use_case 决定 agent 基线与值守模式，
// llm_config_id / skill_ids 是任务级资源冻结。
// 它只被 newTurnContext 消费一次，转成 TurnContext 后不再向下穿透任何函数签名
// ——新增运行维度只需扩展本结构体与注册表，执行器无需改动。
type AssistantRunOptions struct {
	UseCase     string // agent use case（同时是资源链键），空 = chat
	LLMConfigId int64
	SkillIds    []int64
}

// TaskResourceOverrides 定时任务创建时冻结的资源选择——交互轮恒 nil。
// 命名如实：它就是"任务级覆盖配置"，不假装通用（未来 chat 若支持 per-request
// 覆盖再抽象不迟）。
type TaskResourceOverrides struct {
	LLMConfigId int64   // 0 = 跟随链
	SkillIds    []int64 // 空 = 跟随链
}

// TurnContext 一轮执行的完整决策上下文：它是谁（UseCase）、载体实体冻结了什么
// （TaskOverrides）、走哪条业务路径（Handler / Req）、按用例查出的基线（Agent），
// 以及该用例声明的资源链与值守模式（chain）。
// 刻意不叫 "TurnSource"——它不是单一来源，而是本轮资源与行为裁决的全部输入。
// Handler / Req 在流程中逐步补齐（②构造、③填 Handler、③之后填 Req）。
type TurnContext struct {
	UseCase       string
	TaskOverrides *TaskResourceOverrides
	Handler       *chat.ActionHandler
	Req           *chat.AIChatRequest
	Agent         *models.AIAgent

	chain turnResourceChain
}

// Interactive 报告本轮是否有人在环：交互轮给"下一步建议"、走内容路由；无人值守轮
// （定时任务）两项全关。工具面不随值守模式变化（定时任务与交互轮同一套工具），
// 因此中断在两种轮次都可能出现——无人值守轮的中断由人在执行会话里接管。
func (tc *TurnContext) Interactive() bool { return tc.chain.Interactive }

// resolveAction 决定本轮 action：注册表声明了固定 ActionKey 的轮次（无人值守）
// 跳过内容路由——任务正文里的"创建/新建"动词不该被 creation fast-path 接走。
// 交互轮走确定性内容路由。
func (tc *TurnContext) resolveAction(content string, paramCount int, prevRoute *models.ConversationRoute) (string, string) {
	if tc.chain.ActionKey != "" {
		return tc.chain.ActionKey, "fixed"
	}
	return resolveActionKey(content, paramCount, prevRoute)
}

// ==================== use_case 注册表（唯一的语义声明点） ====================

type llmResolver func(*Router, *TurnContext) (*models.AILLMConfig, error)
type skillResolver func(*Router, *TurnContext) *aiagent.SkillConfig

// turnResourceChain 每个 use_case 一条声明：值守模式 + 固定 action + 两类资源的
// 优先级链。链节点按"决策者离本轮意图的远近"排序，第一个命中者生效。
// 新增 use_case = 在这里注册一个条目（+ 按需挑现成节点或写一个新节点），
// 执行器（resolveTurnLLM / resolveTurnSkills / TurnContext.resolveAction）零改动。
type turnResourceChain struct {
	// Interactive 交互轮：GuidedFollowup + 内容路由。工具面不在此列——定时任务与
	// 交互轮用同一套工具（写工具自带缺参门/提案确认门），区别只在 prompt 与路由。
	Interactive bool

	// ActionKey 固定的 action handler；空 = 走 resolveActionKey 内容路由。
	ActionKey string

	LLM    []llmResolver
	Skills []skillResolver
}

// turnResourceChains 是 use_case → 资源链的唯一声明点。两条链的语义差异一眼可读
// （而不是散落在几个 if 里）：
//
//	chat（交互）     LLM 用户控制面 > 运维基线    ｜ 技能 handler 声明 > agent 绑定 > 目录自取
//	cron_task（值守） LLM 任务冻结 > 运维基线 > 全局兜底 ｜ 技能 任务冻结 > agent 绑定 > 目录自取
var turnResourceChains = map[string]turnResourceChain{
	models.UseCaseChat: {
		Interactive: true,
		// 用户控制面压过运维基线："用户标了默认，下轮对话立即切"（原
		// resolveChatLLMConfig 的语义，见 llmGlobalDefault）。
		LLM: []llmResolver{llmGlobalDefault, llmAgentBinding},
		// handler 的空切片是"明确表态不预载"（general_chat 防 agent 绑定膨胀默认
		// 路径 prompt），这个语义必须保留——它只能出现在交互链。
		Skills: []skillResolver{skillsHandlerRequired, skillsAgentBinding, skillsCatalog},
	},
	models.UseCaseCronTask: {
		Interactive: false,
		// 无人值守：任务正文里的创建动词不得触发 creation fast-path，固定走
		// cron_task handler（汇报式 prompt，见 aiagent/chat/actions.go）。工具面与
		// general_chat 相同，只有 prompt 与路由不同。
		ActionKey: string(models.ActionKeyCronTask),
		// 任务行为要稳定：创建者指定的模型绝不能被设置页翻转 is_default 静默改掉；
		// 未指定时跟随 cron_task 基线（运维为该用例定的策略），is_default 只做兜底。
		LLM: []llmResolver{llmTaskOverride, llmAgentBinding, llmGlobalDefault},
		// handler 整节摘除：RequiredSkills 的空切片语义是为交互 prompt 瘦身设的，
		// general_chat 会静默清空 cron_task 基线的技能绑定。
		Skills: []skillResolver{skillsTaskOverride, skillsAgentBinding, skillsCatalog},
	},
}

// ==================== LLM 链节点 ====================

// llmTaskOverride 命中：任务创建时显式指定的模型。值守轮的资源冻结必须精确生效
// （fail-loud），绝不静默回退——全局默认一翻转就把所有任务的模型悄悄换掉，是
// 运维最难发现的一类漂移。
func llmTaskOverride(rt *Router, tc *TurnContext) (*models.AILLMConfig, error) {
	if tc.TaskOverrides == nil || tc.TaskOverrides.LLMConfigId <= 0 {
		return nil, nil // 未命中，链继续
	}

	cfg, err := models.AILLMConfigGetById(rt.Ctx, tc.TaskOverrides.LLMConfigId)
	if err != nil {
		return nil, &turnResolutionError{
			Status: http.StatusInternalServerError,
			Msg:    fmt.Sprintf("load specified LLM config id=%d failed: %v", tc.TaskOverrides.LLMConfigId, err),
		}
	}
	if cfg == nil || !cfg.Enabled {
		logger.Infof("[Assistant] specified LLM config id=%d is missing or disabled", tc.TaskOverrides.LLMConfigId)
		return nil, &turnResolutionError{Status: http.StatusConflict, Msg: noLLMConfiguredMsg}
	}
	return cfg, nil
}

// llmAgentBinding 命中：use_case agent 的运维绑定。查不到/已禁用按未命中处理，
// 交给链的下一节点——与旧 resolveChatLLMConfig 的 agent 兜底分支同容错。
func llmAgentBinding(rt *Router, tc *TurnContext) (*models.AILLMConfig, error) {
	if tc.Agent == nil || tc.Agent.LLMConfigId <= 0 {
		return nil, nil
	}

	cfg, err := models.AILLMConfigGetById(rt.Ctx, tc.Agent.LLMConfigId)
	if err != nil {
		logger.Warningf("[Assistant] load agent LLM config id=%d failed: %v", tc.Agent.LLMConfigId, err)
		return nil, nil
	}
	// AILLMConfigGetById 是通用 getter，故意不过滤 enabled——管理后台编辑页要能
	// 查出已禁用的记录。但用在运行路径上，命中已禁用配置应当视为"agent 没可用 LLM"。
	if cfg == nil || !cfg.Enabled {
		logger.Infof("[Assistant] agent's bound LLM config id=%d is disabled", tc.Agent.LLMConfigId)
		return nil, nil
	}
	return cfg, nil
}

// llmGlobalDefault 命中：全局 is_default（内部已过滤 enabled）；未配置返回 nil
// 自然落空。它在 chat 链排第一的语义：用户在 LLM 配置列表把某条标为"默认"，
// 直觉预期就是对话立刻切到该模型——若 agent 绑定优先，默认标记会被静默忽略，
// 用户很难发现 chat 实际在用 Agent 设置页绑定的另一条配置。
func llmGlobalDefault(rt *Router, _ *TurnContext) (*models.AILLMConfig, error) {
	cfg, err := models.AILLMConfigPickDefault(rt.Ctx)
	if err != nil {
		logger.Warningf("[Assistant] pick default LLM config failed: %v", err)
	}
	return cfg, nil
}

// ==================== 技能链节点 ====================

// skillsHandlerRequired 命中：handler 显式声明了 RequiredSkills 字段。
// 命中语义分两档：nil 函数值 = 未声明 = 未命中；返回空切片 = 明确表态"本次不
// 预载"（交互 prompt 瘦身）——命中即生效。这正是它只能出现在 chat 链、绝不能
// 出现在 cron 链的原因。
func skillsHandlerRequired(rt *Router, tc *TurnContext) *aiagent.SkillConfig {
	if tc.Handler == nil || tc.Handler.RequiredSkills == nil {
		return nil
	}
	names := tc.Handler.RequiredSkills(tc.Req)
	logger.Debugf("[Assistant] action %q declared RequiredSkills=%v, pinned preload", tc.Req.ActionKey, names)
	return &aiagent.SkillConfig{SkillNames: names}
}

// skillsTaskOverride 命中：任务创建时指定的技能预载。任务技能全部失效（禁用/
// 删除）时按未命中处理，交回链的下一节点——与 LLM 侧"任务冻结 fail-loud"不同：
// 技能只是能力清单，失效时回落到运维基线仍然可用。
func skillsTaskOverride(rt *Router, tc *TurnContext) *aiagent.SkillConfig {
	if tc.TaskOverrides == nil || len(tc.TaskOverrides.SkillIds) == 0 {
		return nil
	}

	names, err := models.AISkillNamesByIds(rt.Ctx, tc.TaskOverrides.SkillIds)
	if err != nil {
		logger.Warningf("[Assistant] load task skill names %v failed: %v", tc.TaskOverrides.SkillIds, err)
		return nil
	}
	if len(names) == 0 {
		return nil
	}
	return &aiagent.SkillConfig{SkillNames: names}
}

// skillsAgentBinding 命中：agent 绑定的 SkillIds。DB 失败/全部失效按未命中处理，
// 落到目录自取——与原 buildSkillConfigForAgent 的容错一致。
func skillsAgentBinding(rt *Router, tc *TurnContext) *aiagent.SkillConfig {
	if tc.Agent == nil || len(tc.Agent.SkillIds) == 0 {
		return nil
	}

	names, err := models.AISkillNamesByIds(rt.Ctx, tc.Agent.SkillIds)
	if err != nil {
		logger.Warningf("[Assistant] load skill names for agent=%d failed: %v", tc.Agent.Id, err)
		return nil
	}
	if len(names) == 0 {
		return nil
	}
	return &aiagent.SkillConfig{SkillNames: names}
}

// skillsCatalog 兜底节点：恒命中——不预载，常驻目录 + load_skill 模型自取。
func skillsCatalog(_ *Router, _ *TurnContext) *aiagent.SkillConfig {
	return &aiagent.SkillConfig{}
}

// ==================== 链执行器（机械执行，零 use_case 分支） ====================

// noLLMConfiguredMsg 是"无可用 LLM"的统一文案：前端按 409 + 这条文案弹"去配置"
// 引导（写进 message.ErrCode，见 assistantMessageNew 的状态透传）。
const noLLMConfiguredMsg = "no LLM configured, please configure one in system settings"

// turnResolutionError 让链节点表达"该失败应以什么 HTTP 语义收口"：router 的收尾
// 约定是 (httpStatus, msg)，而链节点只返回 error——用它把状态码带出来，避免每个
// 节点都退化成三元组返回值。未包装的 error 一律按 500 收口。
type turnResolutionError struct {
	Status int
	Msg    string
}

func (e *turnResolutionError) Error() string { return e.Msg }

// resolveTurnLLM 按本轮声明的链逐节点尝试，第一个命中者生效。返回 (nil, status, msg)
// 表示无可用 LLM——409 是前端"去配置"引导的契约码。
func (rt *Router) resolveTurnLLM(tc *TurnContext) (*models.AILLMConfig, int, string) {
	for _, resolve := range tc.chain.LLM {
		cfg, err := resolve(rt, tc)
		if err != nil {
			var tre *turnResolutionError
			if errors.As(err, &tre) {
				return nil, tre.Status, tre.Msg
			}
			return nil, http.StatusInternalServerError, err.Error()
		}
		if cfg != nil {
			return cfg, 0, ""
		}
	}
	return nil, http.StatusConflict, noLLMConfiguredMsg
}

// resolveTurnSkills 按本轮声明的链逐节点尝试，第一个命中者生效。链自带恒命中的
// 兜底节点（skillsCatalog），走完仍未命中只可能是注册表漏了兜底——按兜底语义
// （不预载，目录 + load_skill 自取）返回空配置。
func (rt *Router) resolveTurnSkills(tc *TurnContext) *aiagent.SkillConfig {
	for _, resolve := range tc.chain.Skills {
		if cfg := resolve(rt, tc); cfg != nil {
			return cfg
		}
	}
	return &aiagent.SkillConfig{}
}

// newTurnContext 构造本轮决策上下文：use_case 选 agent 基线（空 = chat），同时
// 锁定该 use_case 声明的资源链与值守模式；任务级覆盖转成 TaskResourceOverrides。
// 未注册的 use_case 回落 chat 链并大声记日志——agent 基线仍按原 use_case 查。
// 返回 (nil, httpStatus, msg) 表示构造失败：400 无可用 agent、500 系统错误。
func (rt *Router) newTurnContext(opts *AssistantRunOptions) (*TurnContext, int, string) {
	useCase := models.UseCaseChat
	if opts != nil && opts.UseCase != "" {
		useCase = opts.UseCase
	}

	agent, err := models.AIAgentGetByUseCase(rt.Ctx, useCase)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Sprintf("load AI agent for use_case=%s failed: %v", useCase, err)
	}
	if agent == nil {
		return nil, http.StatusBadRequest, fmt.Sprintf("no AI agent configured for use_case=%s", useCase)
	}

	chain, ok := turnResourceChains[useCase]
	if !ok {
		logger.Warningf("[Assistant] unregistered use_case=%q, falling back to chat chain", useCase)
		chain = turnResourceChains[models.UseCaseChat]
	}

	tc := &TurnContext{UseCase: useCase, Agent: agent, chain: chain}
	if opts != nil && (opts.LLMConfigId > 0 || len(opts.SkillIds) > 0) {
		tc.TaskOverrides = &TaskResourceOverrides{LLMConfigId: opts.LLMConfigId, SkillIds: opts.SkillIds}
	}
	return tc, 0, ""
}

// chatTurnContextBestEffort 构造一个不做硬门校验的 chat 轮上下文：agent 查不到/
// 查库出错也照常返回（Agent 为 nil 时链自然落到全局默认节点）。供 approval 意图
// 分类这类旁路调用使用——它们的契约是"取不到 LLM 就降级 unclear"，不该因为 agent
// 配置缺失而升级成 400 硬失败。
func (rt *Router) chatTurnContextBestEffort() *TurnContext {
	agent, err := models.AIAgentGetByUseCase(rt.Ctx, models.UseCaseChat)
	if err != nil {
		logger.Warningf("[Assistant] load chat agent: %v", err)
	}
	return &TurnContext{
		UseCase: models.UseCaseChat,
		Agent:   agent,
		chain:   turnResourceChains[models.UseCaseChat],
	}
}
