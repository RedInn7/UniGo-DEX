package api

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"unigo-dex/chain"
	"unigo-dex/indexer"
	umath "unigo-dex/math"
	"unigo-dex/metrics"
	"unigo-dex/mev"
	"unigo-dex/oracle"
	"unigo-dex/router"
)

// trade.go — 价格安全检查、待签名交易构造、TWAP 与套利查询

// priceSafety 一条路由的价格安全检查结果
type priceSafety struct {
	warning  string
	rejected bool             // 交易前检查应当拒绝
	checks   []PoolPriceCheck // 每个池子的校验明细
}

// checkPriceSafety 对路由经过的每个池子做价格安全检查，报价 / 构造交易 / 提交交易共用
//
// 每个池子的现价与两个独立的参考价交叉校验：
//   - Chainlink 喂价：现价偏离喂价过大，拒绝；喂价不可用（过期、轮次未完成等）时不参与校验
//   - 池内 TWAP：现价偏离 TWAP 过大视为疑似单区块操纵，拒绝；取不到时不参与校验
//
// 参考价缺了哪个就少校验一方并在警告里说明，不会拿坏数据去比。
// 另外，三方交叉验证触发保护模式后，滑点不合规的请求拒绝。
func (s *Server) checkPriceSafety(ctx context.Context, route *router.Route, slippage float64) priceSafety {
	var (
		result   priceSafety
		warnings []string
	)
	for _, pool := range route.Pools {
		check := s.checkPool(ctx, pool)
		result.checks = append(result.checks, check)

		switch check.ChainlinkStatus {
		case oracle.ChainlinkDeviated:
			result.rejected = true
			warnings = append(warnings, fmt.Sprintf(
				"⚠️ 池子 %s 的现价偏离 Chainlink 喂价 %.2f%%（阈值 %.2f%%），已拒绝交易。",
				pool.Address, check.ChainlinkDeviation*100, s.oracle.MaxDeviation()*100))
		case oracle.ChainlinkUnavailable:
			warnings = append(warnings, fmt.Sprintf(
				"⚠️ 池子 %s 的 Chainlink 喂价不可用（%s），价格校验已降级，不再参考该喂价。", pool.Address, check.ChainlinkDetail))
		}

		switch check.Status {
		case oracle.TWAPDeviated:
			result.rejected = true
			warnings = append(warnings, fmt.Sprintf(
				"⚠️ 池子 %s 的现价偏离 %d 秒 TWAP %.2f%%，疑似被单区块操纵，已拒绝交易。", pool.Address, check.Window, check.Deviation*100))
		case oracle.TWAPUnavailable:
			if s.chain != nil {
				warnings = append(warnings, fmt.Sprintf(
					"⚠️ 池子 %s 的 TWAP 不可用（%s），无法校验现价是否被操纵。", pool.Address, check.Detail))
			}
		default:
			if check.Degraded {
				warnings = append(warnings, fmt.Sprintf(
					"⚠️ 池子 %s 的预言机观测不足，TWAP 窗口只有 %d 秒（要求 %d 秒），抗操纵能力较弱。",
					pool.Address, check.Window, s.config.TWAPWindow))
			}
		}

		allowed, _, warning := s.oracle.ValidateSwap(oracle.PairKey(pool.Token0, pool.Token1), slippage)
		if !allowed {
			result.rejected = true
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	result.warning = joinWarnings(warnings...)
	return result
}

// checkPool 用 Chainlink 喂价和 TWAP 校验一个池子的现价
func (s *Server) checkPool(ctx context.Context, pool indexer.PoolSnapshot) PoolPriceCheck {
	pair := oracle.PairKey(pool.Token0, pool.Token1)
	spot := umath.SqrtPriceX96ToPrice(pool.SqrtPriceX96)
	check := PoolPriceCheck{Pool: pool.Address, Spot: spot, Sources: []string{"spot"}}

	// Chainlink（先于 TWAP：两者都记入预言机后一起做三方交叉验证）
	chainlink := s.oracle.CheckChainlink(pair, spot, s.chainlink.Read(ctx, pool.Token0, pool.Token1))
	check.ChainlinkStatus, check.Chainlink = chainlink.Status, chainlink.Price
	check.ChainlinkDeviation, check.ChainlinkDetail = chainlink.Deviation, chainlink.Detail
	if chainlink.Status == oracle.ChainlinkOK || chainlink.Status == oracle.ChainlinkDeviated {
		check.Sources = append(check.Sources, "chainlink")
	}

	// TWAP
	if s.chain == nil {
		check.Status, check.Detail = oracle.TWAPUnavailable, "未连接节点"
		return check
	}
	key := fmt.Sprintf("twap:%s:%d", pool.Address, s.idxer.GetStats().LastBlockNumber)
	cached, _ := s.cache.GetOrLoad(key, func() (interface{}, error) {
		twap, err := s.chain.TWAP(ctx, pool.Address, s.config.TWAPWindow, s.config.TWAPMinWindow)
		return twapResult{twap, err}, nil
	})
	reading := cached.(twapResult)
	if reading.err != nil {
		check.Status = s.oracle.CheckTWAP(pair, spot, 0).Status
		check.Detail = "节点查询失败"
		if errors.Is(reading.err, chain.ErrTWAPUnavailable) {
			check.Detail = "预言机观测不足"
		}
		return check
	}
	twap := s.oracle.CheckTWAP(pair, spot, reading.twap.Price)
	check.Status, check.TWAP, check.Deviation = twap.Status, twap.TWAP, twap.Deviation
	check.Tick, check.Window, check.Degraded = reading.twap.Tick, reading.twap.Window, reading.twap.Degraded
	check.Sources = append(check.Sources, "twap")
	return check
}

type twapResult struct {
	twap *chain.TWAP
	err  error
}

// handleTWAP 查询池子的现价校验明细：TWAP、Chainlink 喂价及各自的偏离
func (s *Server) handleTWAP(w http.ResponseWriter, r *http.Request) {
	if s.chain == nil {
		writeError(w, http.StatusServiceUnavailable, "chain node not configured")
		return
	}
	pool, ok := s.state.GetPool(strings.ToLower(r.URL.Query().Get("pool")))
	if !ok {
		writeError(w, http.StatusNotFound, "pool not found")
		return
	}
	writeJSON(w, http.StatusOK, s.checkPool(r.Context(), pool.Snapshot()))
}

// handleNonce 返回地址当前的 EIP-712 nonce，客户端签名 SwapPermit 时使用
func (s *Server) handleNonce(w http.ResponseWriter, r *http.Request) {
	address := r.PathValue("address")
	if !common.IsHexAddress(address) {
		writeError(w, http.StatusBadRequest, "invalid address")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"address": strings.ToLower(address),
		"nonce":   s.verifier.GetNonce(address),
	})
}

// handleArbitrage 当前的套利机会，净利润高的在前
func (s *Server) handleArbitrage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.arb.Opportunities())
}

// handleBuildSwap 把路由编码成 Manager 的调用数据，返回待签名交易
//
//   - 精确输入、单跳：Manager.swap
//   - 精确输入、多跳：Manager.exactInput（紧凑路径 token+fee+token+...）
//   - 精确输出（单跳）：Manager.exactOutputSingle
//
// 交易里带上按滑点算出的 amountOutMinimum / amountInMaximum 和 deadline。
// 这里每次都重新搜索路由（不走缓存），并在配置了 Quoter 时用链上报价做一致性抽检。
func (s *Server) handleBuildSwap(w http.ResponseWriter, r *http.Request) {
	if s.config.ManagerAddress == "" {
		writeError(w, http.StatusServiceUnavailable, "MANAGER_ADDRESS not configured")
		return
	}

	var req BuildSwapRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tokenIn, tokenOut := strings.ToLower(req.TokenIn), strings.ToLower(req.TokenOut)
	if !common.IsHexAddress(tokenIn) || !common.IsHexAddress(tokenOut) || !common.IsHexAddress(req.Recipient) {
		writeError(w, http.StatusBadRequest, "token_in, token_out and recipient must be addresses")
		return
	}
	if req.From != "" && !common.IsHexAddress(req.From) {
		writeError(w, http.StatusBadRequest, "invalid from")
		return
	}
	if (req.AmountIn == "") == (req.AmountOut == "") {
		writeError(w, http.StatusBadRequest, "specify exactly one of amount_in and amount_out")
		return
	}
	now := time.Now()
	if req.Deadline == 0 {
		req.Deadline = now.Add(10 * time.Minute).Unix()
	}

	// 路由
	exactOutput := req.AmountOut != ""
	amountStr := req.AmountIn
	if exactOutput {
		amountStr = req.AmountOut
	}
	amount, ok := new(big.Int).SetString(amountStr, 10)
	if !ok || amount.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "invalid amount")
		return
	}
	var (
		route *router.Route
		err   error
	)
	if exactOutput {
		route, err = s.router.FindExactOutputRoute(tokenIn, tokenOut, amount)
	} else {
		route, err = s.router.FindBestRoute(r.Context(), tokenIn, tokenOut, amount)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "route search failed: "+err.Error())
		return
	}

	// 与提交交易相同的交易前检查
	safety := s.checkPriceSafety(r.Context(), route, req.Slippage)
	if safety.rejected {
		writeError(w, http.StatusForbidden, safety.warning)
		return
	}
	impact := mev.PriceImpact(route.Pools, tokenIn, route.AmountIn, route.AmountOut)
	if err := s.guard.CheckSwap(impact, req.Deadline, now); err != nil {
		writeError(w, http.StatusBadRequest, "swap rejected: "+err.Error())
		return
	}
	slippage, mevWarning := s.guard.RecommendSlippage(poolAddresses(route), req.Slippage, now)

	resp := BuildSwapResponse{
		To:          strings.ToLower(s.config.ManagerAddress),
		Value:       "0",
		AmountIn:    route.AmountIn.String(),
		AmountOut:   route.AmountOut.String(),
		Path:        route.Path,
		Fees:        route.Fees(),
		Slippage:    slippage,
		Deadline:    req.Deadline,
		PriceImpact: impact,
		PriceChecks: safety.checks,
	}
	warnings := []string{safety.warning, mevWarning}

	// 链上一致性抽检：同一路径、同一输入，链上 Quoter 报多少
	quoted := route.AmountOut
	if onchain, ok := s.crossCheckQuote(r.Context(), route); ok {
		consistent := onchain.Cmp(route.AmountOut) == 0
		if exactOutput {
			// 精确输出时链上按算出的输入报价，结果不应低于要求的输出
			consistent = onchain.Cmp(route.AmountOut) >= 0
		}
		resp.OnchainAmountOut, resp.QuoteConsistent = onchain.String(), &consistent
		if !consistent {
			warnings = append(warnings, "⚠️ 链下报价与链上 Quoter 不一致（链下状态可能落后于链上），滑点保护已按较低的一方计算。")
			if onchain.Cmp(quoted) < 0 {
				quoted = onchain
			}
		}
	}

	// 编码调用数据
	var data []byte
	switch {
	case exactOutput:
		maxIn := mev.MaxAmountIn(route.AmountIn, slippage)
		resp.Method, resp.MaxAmountIn = "exactOutputSingle", maxIn.String()
		data, err = chain.EncodeExactOutputSingle(tokenIn, tokenOut, route.Pools[0].Fee, req.Recipient, route.AmountOut, maxIn, req.Deadline)
	case len(route.Pools) == 1:
		minOut := mev.MinAmountOut(quoted, slippage)
		resp.Method, resp.MinAmountOut = "swap", minOut.String()
		pool := route.Pools[0]
		data, err = chain.EncodeSwap(pool.Address, tokenIn == pool.Token0, route.AmountIn, req.Recipient, minOut, req.Deadline)
	default:
		minOut := mev.MinAmountOut(quoted, slippage)
		resp.Method, resp.MinAmountOut = "exactInput", minOut.String()
		var path []byte
		if path, err = chain.EncodePath(route.Path, route.Fees()); err == nil {
			data, err = chain.EncodeExactInput(path, req.Recipient, route.AmountIn, minOut, req.Deadline)
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode failed: "+err.Error())
		return
	}
	resp.Data = hexutil.Encode(data)

	// gas：能问节点就问节点（需要 from 已授权，否则模拟会 revert），问不到用路由引擎的估算
	resp.GasLimit, resp.GasLimitSource = route.GasEstimate, "router"
	if s.chain != nil {
		if id, err := s.chain.ChainID(r.Context()); err == nil {
			resp.ChainID = id.Uint64()
		}
		if req.From != "" {
			if estimated, err := s.chain.EstimateGas(r.Context(), req.From, s.config.ManagerAddress, data); err == nil {
				resp.GasLimit, resp.GasLimitSource = estimated+estimated/5, "node"
			} else {
				warnings = append(warnings, "⚠️ 节点模拟该交易失败（常见原因：from 尚未授权 Manager 或余额不足），gas 为静态估算。")
			}
		}
	}
	resp.Gas = s.gas.Estimate(r.Context(), resp.GasLimit)
	resp.Warning = joinWarnings(warnings...)

	writeJSON(w, http.StatusOK, resp)
}

// crossCheckQuote 用链上 Quoter 对路由做一次报价，并把与链下报价的偏差计入 metrics
//
// 没有配置 Quoter、没有连接节点或调用失败时返回 false。
func (s *Server) crossCheckQuote(ctx context.Context, route *router.Route) (*big.Int, bool) {
	if s.chain == nil || s.config.QuoterAddress == "" {
		return nil, false
	}
	path, err := chain.EncodePath(route.Path, route.Fees())
	if err != nil {
		return nil, false
	}
	onchain, err := s.chain.QuoteExactInput(ctx, s.config.QuoterAddress, path, route.AmountIn)
	if err != nil {
		return nil, false
	}

	metrics.QuoteOnchainChecks.Inc()
	diff := new(big.Int).Sub(onchain, route.AmountOut)
	if diff.Sign() != 0 {
		metrics.QuoteOnchainMismatch.Inc()
	}
	// 偏差（基点）= |链上 - 链下| / 链上 × 10000
	bps := 0.0
	if onchain.Sign() > 0 {
		ratio := new(big.Float).Quo(new(big.Float).SetInt(diff.Abs(diff)), new(big.Float).SetInt(onchain))
		bps, _ = ratio.Float64()
		bps *= 10000
	}
	metrics.QuoteOnchainDeviation.Observe(bps)
	return onchain, true
}
