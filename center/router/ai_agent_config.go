package router

import (
	"github.com/ccfos/nightingale/v6/aiagent"
	"github.com/ccfos/nightingale/v6/models"

	"github.com/toolkits/pkg/logger"
)

// 本轮运行参数（use_case / 资源链 / 任务级覆盖）的解析全部收敛在 router_ai_turn.go：
// 入口构造一次 TurnContext，LLM 与技能各自沿该 use_case 声明的链取值，本文件只保留
// 与"agent 配置"相邻的两件事——私有技能可见性名单、外部工具源接线。

// hiddenSkillNamesForUser 算出请求用户在 AI 对话里无权访问的私有 skill 名字，用于
// 目录展示与 load_skill / run_skill_script / get_skill 加载层的统一拦截——私有
// skill 只对授权团队可见/可用。
//
// 返回 denyAll=true 表示无法算出隐藏名单（fail closed 兜底）：调用方应据此拒绝本轮
// 所有 skill（目录留空 + 拒绝所有按名加载/执行）。
//
// Fail closed 分层：只有「确定是管理员」才返回 (nil,false)（放行全部）；解析用户 /
// 团队的任一步出错或用户不存在，都以空团队集兜底（AISkillHiddenNames(nil) 会把所有
// 私有 skill 计为隐藏）；连私有 skill 列表本身都查不出来时，返回 denyAll=true，绝不
// 把「查询失败」误当成「不过滤」而泄露私有 skill。
func (rt *Router) hiddenSkillNamesForUser(userId int64) (hidden []string, denyAll bool) {
	me, err := models.UserGetById(rt.Ctx, userId)
	if err == nil && me != nil && me.IsAdmin() {
		return nil, false
	}

	var gids []int64
	switch {
	case err != nil:
		logger.Errorf("[AIAgent] load user=%d failed, hiding all private skills: %v", userId, err)
	case me == nil:
		logger.Warningf("[AIAgent] user=%d not found, hiding all private skills", userId)
	default:
		if g, gerr := models.MyGroupIds(rt.Ctx, userId); gerr != nil {
			logger.Errorf("[AIAgent] load groups for user=%d failed, hiding all private skills: %v", userId, gerr)
		} else {
			gids = g
		}
	}

	names, herr := models.AISkillHiddenNames(rt.Ctx, gids)
	if herr != nil {
		// 无法枚举私有 skill：fail closed —— 本轮拒绝所有 skill，而不是放行。
		logger.Errorf("[AIAgent] list private skills for user=%d failed, denying all skills this turn: %v", userId, herr)
		return nil, true
	}
	return names, false
}

// agentToolSources resolves the per-run external tool sources for agent+me via
// AgentToolSourcesHook. The hook is the embedder's extension point (e.g. the
// enterprise edition translates agent.MCPServerIds into MCP client sources,
// applying its own per-user team scoping); unset means no external sources.
func (rt *Router) agentToolSources(agent *models.AIAgent, me *models.User) []aiagent.ToolSource {
	if rt.AgentToolSourcesHook == nil {
		return nil
	}
	return rt.AgentToolSourcesHook(agent, me)
}
