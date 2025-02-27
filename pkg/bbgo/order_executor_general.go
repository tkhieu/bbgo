package bbgo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.uber.org/multierr"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/c9s/bbgo/pkg/util"
)

type NotifyFunc func(obj interface{}, args ...interface{})

// GeneralOrderExecutor implements the general order executor for strategy
type GeneralOrderExecutor struct {
	session            *ExchangeSession
	symbol             string
	strategy           string
	strategyInstanceID string
	position           *types.Position
	activeMakerOrders  *ActiveOrderBook
	orderStore         *OrderStore
	tradeCollector     *TradeCollector

	marginBaseMaxBorrowable, marginQuoteMaxBorrowable fixedpoint.Value
}

func NewGeneralOrderExecutor(session *ExchangeSession, symbol, strategy, strategyInstanceID string, position *types.Position) *GeneralOrderExecutor {
	// Always update the position fields
	position.Strategy = strategy
	position.StrategyInstanceID = strategyInstanceID

	orderStore := NewOrderStore(symbol)

	executor := &GeneralOrderExecutor{
		session:            session,
		symbol:             symbol,
		strategy:           strategy,
		strategyInstanceID: strategyInstanceID,
		position:           position,
		activeMakerOrders:  NewActiveOrderBook(symbol),
		orderStore:         orderStore,
		tradeCollector:     NewTradeCollector(symbol, position, orderStore),
	}

	if session.Margin {
		executor.startMarginAssetUpdater(context.Background())
	}

	return executor
}

func (e *GeneralOrderExecutor) startMarginAssetUpdater(ctx context.Context) {
	marginService, ok := e.session.Exchange.(types.MarginBorrowRepayService)
	if !ok {
		log.Warnf("session %s (%T) exchange does not support MarginBorrowRepayService", e.session.Name, e.session.Exchange)
		return
	}

	go e.marginAssetMaxBorrowableUpdater(ctx, 30*time.Minute, marginService, e.position.Market)
}

func (e *GeneralOrderExecutor) marginAssetMaxBorrowableUpdater(ctx context.Context, interval time.Duration, marginService types.MarginBorrowRepayService, market types.Market) {
	t1 := time.NewTicker(util.MillisecondsJitter(30*time.Minute, 500))
	t2 := time.NewTicker(util.MillisecondsJitter(30*time.Minute, 500))
	defer t1.Stop()
	defer t2.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-t1.C:
			maxBorrowable, err := marginService.QueryMarginAssetMaxBorrowable(ctx, market.BaseCurrency)
			if err != nil {
				log.WithError(err).Errorf("can not query margin base asset max borrowable")
				continue
			}

			log.Infof("updating margin base asset %s max borrowable amount: %f", market.BaseCurrency, maxBorrowable.Float64())
			e.marginBaseMaxBorrowable = maxBorrowable

		case <-t2.C:
			maxBorrowable, err := marginService.QueryMarginAssetMaxBorrowable(ctx, market.QuoteCurrency)
			if err != nil {
				log.WithError(err).Errorf("can not query margin base asset max borrowable")
				continue
			}

			log.Infof("updating margin quote asset %s max borrowable amount: %f", market.QuoteCurrency, maxBorrowable.Float64())
			e.marginQuoteMaxBorrowable = maxBorrowable
		}
	}
}

func (e *GeneralOrderExecutor) ActiveMakerOrders() *ActiveOrderBook {
	return e.activeMakerOrders
}

func (e *GeneralOrderExecutor) BindEnvironment(environ *Environment) {
	e.tradeCollector.OnProfit(func(trade types.Trade, profit *types.Profit) {
		environ.RecordPosition(e.position, trade, profit)
	})
}

func (e *GeneralOrderExecutor) BindTradeStats(tradeStats *types.TradeStats) {
	e.tradeCollector.OnProfit(func(trade types.Trade, profit *types.Profit) {
		if profit == nil {
			return
		}

		tradeStats.Add(profit)
	})
}

func (e *GeneralOrderExecutor) BindProfitStats(profitStats *types.ProfitStats) {
	e.tradeCollector.OnProfit(func(trade types.Trade, profit *types.Profit) {
		profitStats.AddTrade(trade)
		if profit == nil {
			return
		}

		profitStats.AddProfit(*profit)

		Notify(profit)
		Notify(profitStats)
	})
}

func (e *GeneralOrderExecutor) Bind() {
	e.activeMakerOrders.BindStream(e.session.UserDataStream)
	e.orderStore.BindStream(e.session.UserDataStream)

	// trade notify
	e.tradeCollector.OnTrade(func(trade types.Trade, profit, netProfit fixedpoint.Value) {
		Notify(trade)
	})

	e.tradeCollector.OnPositionUpdate(func(position *types.Position) {
		log.Infof("position changed: %s", position)
		Notify(position)
	})

	e.tradeCollector.BindStream(e.session.UserDataStream)
}

// CancelOrders cancels the given order objects directly
func (e *GeneralOrderExecutor) CancelOrders(ctx context.Context, orders ...types.Order) error {
	err := e.session.Exchange.CancelOrders(ctx, orders...)
	if err != nil { // Retry once
		err = e.session.Exchange.CancelOrders(ctx, orders...)
	}
	return err
}

func (e *GeneralOrderExecutor) SubmitOrders(ctx context.Context, submitOrders ...types.SubmitOrder) (types.OrderSlice, error) {
	formattedOrders, err := e.session.FormatOrders(submitOrders)
	if err != nil {
		return nil, err
	}

	createdOrders, errIdx, err := BatchPlaceOrder(ctx, e.session.Exchange, formattedOrders...)
	if len(errIdx) > 0 {
		createdOrders2, err2 := BatchRetryPlaceOrder(ctx, e.session.Exchange, errIdx, formattedOrders...)
		if err2 != nil {
			err = multierr.Append(err, err2)
		} else {
			createdOrders = append(createdOrders, createdOrders2...)
		}
	}

	e.orderStore.Add(createdOrders...)
	e.activeMakerOrders.Add(createdOrders...)
	e.tradeCollector.Process()
	return createdOrders, err
}

type OpenPositionOptions struct {
	// Long is for open a long position
	// Long or Short must be set, avoid loading it from the config file
	// it should be set from the strategy code
	Long bool `json:"-" yaml:"-"`

	// Short is for open a short position
	// Long or Short must be set
	Short bool `json:"-" yaml:"-"`

	// Leverage is used for leveraged position and account
	// Leverage is not effected when using non-leverage spot account
	Leverage fixedpoint.Value `json:"leverage,omitempty"`

	// Quantity will be used first, it will override the leverage if it's given
	Quantity fixedpoint.Value `json:"quantity,omitempty"`

	// MarketOrder set to true to open a position with a market order
	// default is MarketOrder = true
	MarketOrder bool `json:"marketOrder,omitempty"`

	// LimitOrder set to true to open a position with a limit order
	LimitOrder bool `json:"limitOrder,omitempty"`

	// LimitOrderTakerRatio is used when LimitOrder = true, it adjusts the price of the limit order with a ratio.
	// So you can ensure that the limit order can be a taker order. Higher the ratio, higher the chance it could be a taker order.
	//
	// limitOrderTakerRatio is the price ratio to adjust your limit order as a taker order. e.g., 0.1%
	// for sell order, 0.1% ratio means your final price = price * (1 - 0.1%)
	// for buy order, 0.1% ratio means your final price = price * (1 + 0.1%)
	// this is only enabled when the limitOrder option set to true
	LimitOrderTakerRatio fixedpoint.Value `json:"limitOrderTakerRatio,omitempty"`

	Price fixedpoint.Value `json:"-" yaml:"-"`
	Tags  []string         `json:"-" yaml:"-"`
}

func (e *GeneralOrderExecutor) OpenPosition(ctx context.Context, options OpenPositionOptions) error {
	price := options.Price
	submitOrder := types.SubmitOrder{
		Symbol:           e.position.Symbol,
		Type:             types.OrderTypeMarket,
		MarginSideEffect: types.SideEffectTypeMarginBuy,
		Tag:              strings.Join(options.Tags, ","),
	}

	if !options.LimitOrderTakerRatio.IsZero() {
		if options.Long {
			// use higher price to buy (this ensures that our order will be filled)
			price = price.Mul(one.Add(options.LimitOrderTakerRatio))
		} else if options.Short {
			// use lower price to sell (this ensures that our order will be filled)
			price = price.Mul(one.Sub(options.LimitOrderTakerRatio))
		}
	}

	if options.MarketOrder {
		submitOrder.Type = types.OrderTypeMarket
	} else if options.LimitOrder {
		submitOrder.Type = types.OrderTypeLimit
		submitOrder.Price = price
	}

	quantity := options.Quantity

	if options.Long {
		if quantity.IsZero() {
			quoteQuantity, err := CalculateQuoteQuantity(ctx, e.session, e.position.QuoteCurrency, options.Leverage)
			if err != nil {
				return err
			}

			quantity = quoteQuantity.Div(price)
		}

		submitOrder.Side = types.SideTypeBuy
		submitOrder.Quantity = quantity

		Notify("Opening %s long position with quantity %f at price %f", e.position.Symbol, quantity.Float64(), price.Float64())
		createdOrder, err2 := e.SubmitOrders(ctx, submitOrder)
		if err2 != nil {
			return err2
		}
		_ = createdOrder
		return nil
	} else if options.Short {
		if quantity.IsZero() {
			var err error
			quantity, err = CalculateBaseQuantity(e.session, e.position.Market, price, quantity, options.Leverage)
			if err != nil {
				return err
			}
		}

		submitOrder.Side = types.SideTypeSell
		submitOrder.Quantity = quantity

		Notify("Opening %s short position with quantity %f at price %f", e.position.Symbol, quantity.Float64(), price.Float64())
		createdOrder, err2 := e.SubmitOrders(ctx, submitOrder)
		if err2 != nil {
			return err2
		}
		_ = createdOrder
		return nil
	}

	return errors.New("options Long or Short must be set")
}

// GracefulCancelActiveOrderBook cancels the orders from the active orderbook.
func (e *GeneralOrderExecutor) GracefulCancelActiveOrderBook(ctx context.Context, activeOrders *ActiveOrderBook) error {
	if activeOrders.NumOfOrders() == 0 {
		return nil
	}
	if err := activeOrders.GracefulCancel(ctx, e.session.Exchange); err != nil {
		// Retry once
		if err = activeOrders.GracefulCancel(ctx, e.session.Exchange); err != nil {
			return fmt.Errorf("graceful cancel order error: %w", err)
		}
	}

	e.tradeCollector.Process()
	return nil
}

func (e *GeneralOrderExecutor) GracefulCancelOrder(ctx context.Context, order types.Order) error {
	if e.activeMakerOrders.NumOfOrders() == 0 {
		return nil
	}
	if err := e.activeMakerOrders.Cancel(ctx, e.session.Exchange, order); err != nil {
		// Retry once
		if err = e.activeMakerOrders.Cancel(ctx, e.session.Exchange, order); err != nil {
			return fmt.Errorf("cancel order error: %w", err)
		}
	}
	e.tradeCollector.Process()
	return nil
}

// GracefulCancel cancels all active maker orders
func (e *GeneralOrderExecutor) GracefulCancel(ctx context.Context) error {
	return e.GracefulCancelActiveOrderBook(ctx, e.activeMakerOrders)
}

// ClosePosition closes the current position by a percentage.
// percentage 0.1 means close 10% position
// tag is the order tag you want to attach, you may pass multiple tags, the tags will be combined into one tag string by commas.
func (e *GeneralOrderExecutor) ClosePosition(ctx context.Context, percentage fixedpoint.Value, tags ...string) error {
	submitOrder := e.position.NewMarketCloseOrder(percentage)
	if submitOrder == nil {
		return nil
	}

	// check base balance and adjust the close position order
	if e.position.IsLong() {
		if baseBalance, ok := e.session.Account.Balance(e.position.Market.BaseCurrency); ok {
			submitOrder.Quantity = fixedpoint.Min(submitOrder.Quantity, baseBalance.Available)
		}
		if submitOrder.Quantity.IsZero() {
			return fmt.Errorf("insufficient base balance, can not sell: %+v", submitOrder)
		}
	}

	tagStr := strings.Join(tags, ",")
	submitOrder.Tag = tagStr

	Notify("closing %s position %s with tags: %v", e.symbol, percentage.Percentage(), tagStr)

	_, err := e.SubmitOrders(ctx, *submitOrder)
	return err
}

func (e *GeneralOrderExecutor) TradeCollector() *TradeCollector {
	return e.tradeCollector
}

func (e *GeneralOrderExecutor) Session() *ExchangeSession {
	return e.session
}

func (e *GeneralOrderExecutor) Position() *types.Position {
	return e.position
}

// This implements PositionReader interface
func (e *GeneralOrderExecutor) CurrentPosition() *types.Position {
	return e.position
}

// This implements PositionResetter interface
func (e *GeneralOrderExecutor) ResetPosition() error {
	e.position.Reset()
	return nil
}
