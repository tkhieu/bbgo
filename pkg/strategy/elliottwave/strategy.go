package elliottwave

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/datatype/floats"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/indicator"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/c9s/bbgo/pkg/util"
	"github.com/sirupsen/logrus"
)

const ID = "elliottwave"

var log = logrus.WithField("strategy", ID)
var Two fixedpoint.Value = fixedpoint.NewFromInt(2)
var Three fixedpoint.Value = fixedpoint.NewFromInt(3)
var Four fixedpoint.Value = fixedpoint.NewFromInt(4)
var Delta fixedpoint.Value = fixedpoint.NewFromFloat(0.00001)

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type SourceFunc func(*types.KLine) fixedpoint.Value

type Strategy struct {
	Symbol string `json:"symbol"`

	bbgo.StrategyController
	bbgo.SourceSelector
	types.Market
	Session *bbgo.ExchangeSession

	Interval       types.Interval   `json:"interval"`
	Stoploss       fixedpoint.Value `json:"stoploss"`
	WindowATR      int              `json:"windowATR"`
	WindowQuick    int              `json:"windowQuick"`
	WindowSlow     int              `json:"windowSlow"`
	PendingMinutes int              `json:"pendingMinutes"`
	UseHeikinAshi  bool             `json:"useHeikinAshi"`

	// whether to draw graph or not by the end of backtest
	DrawGraph          bool   `json:"drawGraph"`
	GraphIndicatorPath string `json:"graphIndicatorPath"`
	GraphPNLPath       string `json:"graphPNLPath"`
	GraphCumPNLPath    string `json:"graphCumPNLPath"`

	*bbgo.Environment
	*bbgo.GeneralOrderExecutor
	*types.Position    `persistence:"position"`
	*types.ProfitStats `persistence:"profit_stats"`
	*types.TradeStats  `persistence:"trade_stats"`

	ewo        *ElliottWave
	atr        *indicator.ATR
	heikinAshi *HeikinAshi

	priceLines *types.Queue

	getLastPrice func() fixedpoint.Value

	// for smart cancel
	orderPendingCounter map[uint64]int
	startTime           time.Time
	minutesCounter      int

	// for position
	buyPrice     float64 `persistence:"buy_price"`
	sellPrice    float64 `persistence:"sell_price"`
	highestPrice float64 `persistence:"highest_price"`
	lowestPrice  float64 `persistence:"lowest_price"`

	TrailingCallbackRate    []float64          `json:"trailingCallbackRate"`
	TrailingActivationRatio []float64          `json:"trailingActivationRatio"`
	ExitMethods             bbgo.ExitMethodSet `json:"exits"`

	midPrice fixedpoint.Value
	lock     sync.RWMutex `ignore:"true"`
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	return fmt.Sprintf("%s:%s:%v", ID, s.Symbol, bbgo.IsBackTesting)
}

func (s *Strategy) Subscribe(session *bbgo.ExchangeSession) {
	// by default, bbgo only pre-subscribe 1000 klines.
	// this is not enough if we're subscribing 30m intervals using SerialMarketDataStore
	bbgo.KLinePreloadLimit = int64((s.Interval.Minutes()*s.WindowSlow/1000 + 1) + 1000)
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{
		Interval: types.Interval1m,
	})
	if !bbgo.IsBackTesting {
		session.Subscribe(types.BookTickerChannel, s.Symbol, types.SubscribeOptions{})
	}
	s.ExitMethods.SetAndSubscribe(session, s)
}

func (s *Strategy) CurrentPosition() *types.Position {
	return s.Position
}

func (s *Strategy) ClosePosition(ctx context.Context, percentage fixedpoint.Value) error {
	order := s.Position.NewMarketCloseOrder(percentage)
	if order == nil {
		return nil
	}
	order.Tag = "close"
	order.TimeInForce = ""
	balances := s.GeneralOrderExecutor.Session().GetAccount().Balances()
	baseBalance := balances[s.Market.BaseCurrency].Available
	price := s.getLastPrice()
	if order.Side == types.SideTypeBuy {
		quoteAmount := balances[s.Market.QuoteCurrency].Available.Div(price)
		if order.Quantity.Compare(quoteAmount) > 0 {
			order.Quantity = quoteAmount
		}
	} else if order.Side == types.SideTypeSell && order.Quantity.Compare(baseBalance) > 0 {
		order.Quantity = baseBalance
	}
	for {
		if s.Market.IsDustQuantity(order.Quantity, price) {
			return nil
		}
		_, err := s.GeneralOrderExecutor.SubmitOrders(ctx, *order)
		if err != nil {
			order.Quantity = order.Quantity.Mul(fixedpoint.One.Sub(Delta))
			continue
		}
		return nil
	}

}

func (s *Strategy) initIndicators(store *bbgo.SerialMarketDataStore) error {
	s.priceLines = types.NewQueue(300)
	maSlow := &indicator.SMA{IntervalWindow: types.IntervalWindow{Interval: s.Interval, Window: s.WindowSlow}}
	maQuick := &indicator.SMA{IntervalWindow: types.IntervalWindow{Interval: s.Interval, Window: s.WindowQuick}}
	s.ewo = &ElliottWave{
		maSlow, maQuick,
	}
	s.atr = &indicator.ATR{IntervalWindow: types.IntervalWindow{Interval: s.Interval, Window: s.WindowATR}}
	klines, ok := store.KLinesOfInterval(s.Interval)
	klineLength := len(*klines)
	if !ok || klineLength == 0 {
		return errors.New("klines not exists")
	}
	tmpK := (*klines)[klineLength-1]
	s.startTime = tmpK.StartTime.Time().Add(tmpK.Interval.Duration())
	s.heikinAshi = NewHeikinAshi(500)

	for _, kline := range *klines {
		source := s.GetSource(&kline).Float64()
		s.ewo.Update(source)
		s.atr.PushK(kline)
		s.priceLines.Update(source)
		s.heikinAshi.Update(kline)
	}
	return nil
}

// FIXME: stdevHigh
func (s *Strategy) smartCancel(ctx context.Context, pricef float64) int {
	nonTraded := s.GeneralOrderExecutor.ActiveMakerOrders().Orders()
	if len(nonTraded) > 0 {
		left := 0
		for _, order := range nonTraded {
			if order.Status != types.OrderStatusNew && order.Status != types.OrderStatusPartiallyFilled {
				continue
			}
			log.Warnf("%v | counter: %d, system: %d", order, s.orderPendingCounter[order.OrderID], s.minutesCounter)
			toCancel := false
			if s.minutesCounter-s.orderPendingCounter[order.OrderID] >= s.PendingMinutes {
				toCancel = true
			} else if order.Side == types.SideTypeBuy {
				if order.Price.Float64()+s.atr.Last()*2 <= pricef {
					toCancel = true
				}
			} else if order.Side == types.SideTypeSell {
				// 75% of the probability
				if order.Price.Float64()-s.atr.Last()*2 >= pricef {
					toCancel = true
				}
			} else {
				panic("not supported side for the order")
			}
			if toCancel {
				err := s.GeneralOrderExecutor.GracefulCancelOrder(ctx, order)
				if err == nil {
					delete(s.orderPendingCounter, order.OrderID)
				} else {
					log.WithError(err).Errorf("failed to cancel %v", order.OrderID)
				}
				log.Warnf("cancel %v", order.OrderID)
			} else {
				left += 1
			}
		}
		return left
	}
	return len(nonTraded)
}

func (s *Strategy) trailingCheck(price float64, direction string) bool {
	if s.highestPrice > 0 && s.highestPrice < price {
		s.highestPrice = price
	}
	if s.lowestPrice > 0 && s.lowestPrice > price {
		s.lowestPrice = price
	}
	isShort := direction == "short"
	for i := len(s.TrailingCallbackRate) - 1; i >= 0; i-- {
		trailingCallbackRate := s.TrailingCallbackRate[i]
		trailingActivationRatio := s.TrailingActivationRatio[i]
		if isShort {
			if (s.sellPrice-s.lowestPrice)/s.lowestPrice > trailingActivationRatio {
				return (price-s.lowestPrice)/s.lowestPrice > trailingCallbackRate
			}
		} else {
			if (s.highestPrice-s.buyPrice)/s.buyPrice > trailingActivationRatio {
				return (s.highestPrice-price)/price > trailingCallbackRate
			}
		}
	}
	return false
}

func (s *Strategy) initTickerFunctions() {
	if s.IsBackTesting() {
		s.getLastPrice = func() fixedpoint.Value {
			lastPrice, ok := s.Session.LastPrice(s.Symbol)
			if !ok {
				log.Error("cannot get lastprice")
			}
			return lastPrice
		}
	} else {
		s.Session.MarketDataStream.OnBookTickerUpdate(func(ticker types.BookTicker) {
			bestBid := ticker.Buy
			bestAsk := ticker.Sell
			if !util.TryLock(&s.lock) {
				return
			}
			if !bestAsk.IsZero() && !bestBid.IsZero() {
				s.midPrice = bestAsk.Add(bestBid).Div(Two)
			} else if !bestAsk.IsZero() {
				s.midPrice = bestAsk
			} else if !bestBid.IsZero() {
				s.midPrice = bestBid
			}
			s.lock.Unlock()
		})
		s.getLastPrice = func() (lastPrice fixedpoint.Value) {
			var ok bool
			s.lock.RLock()
			defer s.lock.RUnlock()
			if s.midPrice.IsZero() {
				lastPrice, ok = s.Session.LastPrice(s.Symbol)
				if !ok {
					log.Error("cannot get lastprice")
					return lastPrice
				}
			} else {
				lastPrice = s.midPrice
			}
			return lastPrice
		}
	}
}

func (s *Strategy) Run(ctx context.Context, orderExecutor bbgo.OrderExecutor, session *bbgo.ExchangeSession) error {
	instanceID := s.InstanceID()
	if s.Position == nil {
		s.Position = types.NewPositionFromMarket(s.Market)
	}
	if s.ProfitStats == nil {
		s.ProfitStats = types.NewProfitStats(s.Market)
	}
	if s.TradeStats == nil {
		s.TradeStats = types.NewTradeStats(s.Symbol)
	}
	// StrategyController
	s.Status = types.StrategyStatusRunning
	s.OnSuspend(func() {
		_ = s.GeneralOrderExecutor.GracefulCancel(ctx)
	})
	s.OnEmergencyStop(func() {
		_ = s.GeneralOrderExecutor.GracefulCancel(ctx)
		_ = s.ClosePosition(ctx, fixedpoint.One)
	})
	s.GeneralOrderExecutor = bbgo.NewGeneralOrderExecutor(session, s.Symbol, ID, instanceID, s.Position)
	s.GeneralOrderExecutor.BindEnvironment(s.Environment)
	s.GeneralOrderExecutor.BindProfitStats(s.ProfitStats)
	s.GeneralOrderExecutor.BindTradeStats(s.TradeStats)
	s.GeneralOrderExecutor.TradeCollector().OnPositionUpdate(func(p *types.Position) {
		bbgo.Sync(s)
	})
	s.GeneralOrderExecutor.Bind()

	s.orderPendingCounter = make(map[uint64]int)
	s.minutesCounter = 0

	for _, method := range s.ExitMethods {
		method.Bind(session, s.GeneralOrderExecutor)
	}
	profit := floats.Slice{1., 1.}
	price, _ := s.Session.LastPrice(s.Symbol)
	initAsset := s.CalcAssetValue(price).Float64()
	cumProfit := floats.Slice{initAsset, initAsset}
	modify := func(p float64) float64 {
		return p
	}
	s.GeneralOrderExecutor.TradeCollector().OnTrade(func(trade types.Trade, _profit, _netProfit fixedpoint.Value) {
		price := trade.Price.Float64()
		if s.buyPrice > 0 {
			profit.Update(modify(price / s.buyPrice))
			cumProfit.Update(s.CalcAssetValue(trade.Price).Float64())
		} else if s.sellPrice > 0 {
			profit.Update(modify(s.sellPrice / price))
			cumProfit.Update(s.CalcAssetValue(trade.Price).Float64())
		}
		if s.Position.IsDust(trade.Price) {
			s.buyPrice = 0
			s.sellPrice = 0
			s.highestPrice = 0
			s.lowestPrice = 0
		} else if s.Position.IsLong() {
			s.buyPrice = price
			s.sellPrice = 0
			s.highestPrice = s.buyPrice
			s.lowestPrice = 0
		} else {
			s.sellPrice = price
			s.buyPrice = 0
			s.highestPrice = 0
			s.lowestPrice = s.sellPrice
		}
	})
	s.initTickerFunctions()

	startTime := s.Environment.StartTime()
	s.TradeStats.SetIntervalProfitCollector(types.NewIntervalProfitCollector(types.Interval1d, startTime))
	s.TradeStats.SetIntervalProfitCollector(types.NewIntervalProfitCollector(types.Interval1w, startTime))

	// event trigger order: s.Interval => Interval1m
	store, ok := session.SerialMarketDataStore(s.Symbol, []types.Interval{s.Interval, types.Interval1m})
	if !ok {
		panic("cannot get 1m history")
	}
	if err := s.initIndicators(store); err != nil {
		log.WithError(err).Errorf("initIndicator failed")
		return nil
	}
	s.InitDrawCommands(store, &profit, &cumProfit)
	store.OnKLineClosed(func(kline types.KLine) {
		s.minutesCounter = int(kline.StartTime.Time().Add(kline.Interval.Duration()).Sub(s.startTime).Minutes())
		if kline.Interval == types.Interval1m {
			s.klineHandler1m(ctx, kline)
		} else if kline.Interval == s.Interval {
			s.klineHandler(ctx, kline)
		}
	})

	bbgo.OnShutdown(func(ctx context.Context, wg *sync.WaitGroup) {
		var buffer bytes.Buffer
		for _, daypnl := range s.TradeStats.IntervalProfits[types.Interval1d].GetNonProfitableIntervals() {
			fmt.Fprintf(&buffer, "%s\n", daypnl)
		}
		fmt.Fprintln(&buffer, s.TradeStats.BriefString())
		os.Stdout.Write(buffer.Bytes())
		if s.DrawGraph {
			s.Draw(store, &profit, &cumProfit)
		}
		wg.Done()
	})
	return nil
}

func (s *Strategy) CalcAssetValue(price fixedpoint.Value) fixedpoint.Value {
	balances := s.Session.GetAccount().Balances()
	return balances[s.Market.BaseCurrency].Total().Mul(price).Add(balances[s.Market.QuoteCurrency].Total())
}

func (s *Strategy) klineHandler1m(ctx context.Context, kline types.KLine) {
	if s.Status != types.StrategyStatusRunning {
		return
	}

	stoploss := s.Stoploss.Float64()
	price := s.getLastPrice()
	pricef := price.Float64()
	atr := s.atr.Last()

	numPending := s.smartCancel(ctx, pricef)
	if numPending > 0 {
		log.Infof("pending orders: %d, exit", numPending)
		return
	}
	lowf := math.Min(kline.Low.Float64(), pricef)
	highf := math.Max(kline.High.Float64(), pricef)
	if s.lowestPrice > 0 && lowf < s.lowestPrice {
		s.lowestPrice = lowf
	}
	if s.highestPrice > 0 && highf > s.highestPrice {
		s.highestPrice = highf
	}
	exitShortCondition := s.sellPrice > 0 && (s.sellPrice*(1.+stoploss) <= highf || s.sellPrice+atr <= highf ||
		s.trailingCheck(highf, "short"))
	exitLongCondition := s.buyPrice > 0 && (s.buyPrice*(1.-stoploss) >= lowf || s.buyPrice-atr >= lowf ||
		s.trailingCheck(lowf, "long"))

	if exitShortCondition || exitLongCondition {
		_ = s.ClosePosition(ctx, fixedpoint.One)
	}
}

func (s *Strategy) klineHandler(ctx context.Context, kline types.KLine) {
	s.heikinAshi.Update(kline)
	source := s.GetSource(&kline)
	sourcef := source.Float64()
	s.priceLines.Update(sourcef)
	if s.UseHeikinAshi {
		source := s.GetSource(s.heikinAshi.Last())
		sourcef := source.Float64()
		s.ewo.Update(sourcef)
	} else {
		s.ewo.Update(sourcef)
	}
	s.atr.PushK(kline)

	if s.Status != types.StrategyStatusRunning {
		return
	}

	stoploss := s.Stoploss.Float64()
	price := s.getLastPrice()
	pricef := price.Float64()
	lowf := math.Min(kline.Low.Float64(), pricef)
	highf := math.Min(kline.High.Float64(), pricef)

	s.smartCancel(ctx, pricef)

	atr := s.atr.Last()
	ewo := types.Array(s.ewo, 4)
	if len(ewo) < 4 {
		return
	}
	bull := kline.Close.Compare(kline.Open) > 0

	balances := s.GeneralOrderExecutor.Session().GetAccount().Balances()
	bbgo.Notify("source: %.4f, price: %.4f lowest: %.4f highest: %.4f sp %.4f bp %.4f", sourcef, pricef, s.lowestPrice, s.highestPrice, s.sellPrice, s.buyPrice)
	bbgo.Notify("balances: [Total] %v %s [Base] %s(%v %s) [Quote] %s",
		s.CalcAssetValue(price),
		s.Market.QuoteCurrency,
		balances[s.Market.BaseCurrency].String(),
		balances[s.Market.BaseCurrency].Total().Mul(price),
		s.Market.QuoteCurrency,
		balances[s.Market.QuoteCurrency].String(),
	)

	shortCondition := ewo[0] < ewo[1] && ewo[1] >= ewo[2] && (ewo[1] <= ewo[2] || ewo[2] >= ewo[3]) || s.sellPrice == 0 && ewo[0] < ewo[1] && ewo[1] < ewo[2]
	longCondition := ewo[0] > ewo[1] && ewo[1] <= ewo[2] && (ewo[1] >= ewo[2] || ewo[2] <= ewo[3]) || s.buyPrice == 0 && ewo[0] > ewo[1] && ewo[1] > ewo[2]

	exitShortCondition := s.sellPrice > 0 && !shortCondition && s.sellPrice*(1.+stoploss) <= highf || s.sellPrice+atr <= highf || s.trailingCheck(highf, "short")
	exitLongCondition := s.buyPrice > 0 && !longCondition && s.buyPrice*(1.-stoploss) >= lowf || s.buyPrice-atr >= lowf || s.trailingCheck(lowf, "long")

	if exitShortCondition || exitLongCondition {
		if err := s.GeneralOrderExecutor.GracefulCancel(ctx); err != nil {
			log.WithError(err).Errorf("cannot cancel orders")
			return
		}
		s.ClosePosition(ctx, fixedpoint.One)
	}
	if longCondition && bull {
		if err := s.GeneralOrderExecutor.GracefulCancel(ctx); err != nil {
			log.WithError(err).Errorf("cannot cancel orders")
			return
		}
		if source.Compare(price) > 0 {
			source = price
			sourcef = source.Float64()
		}
		balances := s.GeneralOrderExecutor.Session().GetAccount().Balances()
		quoteBalance, ok := balances[s.Market.QuoteCurrency]
		if !ok {
			log.Errorf("unable to get quoteCurrency")
			return
		}
		if s.Market.IsDustQuantity(
			quoteBalance.Available.Div(source), source) {
			return
		}
		quantity := quoteBalance.Available.Div(source)
		createdOrders, err := s.GeneralOrderExecutor.SubmitOrders(ctx, types.SubmitOrder{
			Symbol:   s.Symbol,
			Side:     types.SideTypeBuy,
			Type:     types.OrderTypeLimit,
			Price:    source,
			Quantity: quantity,
			Tag:      "long",
		})
		if err != nil {
			log.WithError(err).Errorf("cannot place buy order")
			log.Errorf("%v %v %v", quoteBalance, source, kline)
			return
		}
		s.orderPendingCounter[createdOrders[0].OrderID] = s.minutesCounter
		return
	}
	if shortCondition && !bull {
		if err := s.GeneralOrderExecutor.GracefulCancel(ctx); err != nil {
			log.WithError(err).Errorf("cannot cancel orders")
			return
		}
		if source.Compare(price) < 0 {
			source = price
			sourcef = price.Float64()
		}
		balances := s.GeneralOrderExecutor.Session().GetAccount().Balances()
		baseBalance, ok := balances[s.Market.BaseCurrency]
		if !ok {
			log.Errorf("unable to get baseCurrency")
			return
		}
		if s.Market.IsDustQuantity(baseBalance.Available, source) {
			return
		}
		quantity := baseBalance.Available
		createdOrders, err := s.GeneralOrderExecutor.SubmitOrders(ctx, types.SubmitOrder{
			Symbol:   s.Symbol,
			Side:     types.SideTypeSell,
			Type:     types.OrderTypeLimit,
			Price:    source,
			Quantity: quantity,
			Tag:      "short",
		})
		if err != nil {
			log.WithError(err).Errorf("cannot place sell order")
			return
		}
		s.orderPendingCounter[createdOrders[0].OrderID] = s.minutesCounter
		return
	}
}
