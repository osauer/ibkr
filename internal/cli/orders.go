package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runOrders(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runOrdersOpen(ctx, env, args)
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "open":
		return runOrdersOpen(ctx, env, args)
	default:
		return fail(env, "orders: unknown subcommand %q (try `ibkr orders open`)", sub)
	}
}

func runOrdersOpen(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "orders")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "open" {
		rest = rest[1:]
	}
	if len(rest) != 0 {
		return fail(env, "orders open: usage is `ibkr orders open [--json]`")
	}
	var res rpc.OrdersOpenResult
	if err := env.Conn.Call(ctx, rpc.MethodOrdersOpen, rpc.OrdersOpenParams{}, &res); err != nil {
		return fail(env, "orders open: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrdersOpenText(env, &res)
	return 0
}

func runOrderStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail(env, "order status: usage is `ibkr order status <order-ref|order-id|perm-id>`")
	}
	var res rpc.OrderStatusResult
	if err := env.Conn.Call(ctx, rpc.MethodOrderStatus, rpc.OrderStatusParams{ID: strings.TrimSpace(rest[0])}, &res); err != nil {
		return fail(env, "order status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrderStatusText(env, &res, rest[0])
	return 0
}

func renderOrdersOpenText(env *Env, res *rpc.OrdersOpenResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Open Orders  %s\n", env.statusBadge(statusConcern{Text: strconv.Itoa(len(res.Orders)), Level: statusConcernNone}))
	if len(res.Orders) == 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "No locally tracked open orders.")
		fmt.Fprintln(out)
		return
	}
	for _, order := range res.Orders {
		fmt.Fprintf(out, "  %s\n", formatOrderViewTitle(order))
		fmt.Fprintf(out, "    %s  %s  updated %s\n", order.LifecycleStatus, nonEmpty(order.Status, order.LastEvent), formatOrderTime(order.UpdatedAt))
		if order.LastMessage != "" {
			fmt.Fprintf(out, "    %s\n", order.LastMessage)
		}
		if order.WhyHeld != "" {
			fmt.Fprintf(out, "    held: %s\n", order.WhyHeld)
		}
		if order.MktCapPrice != 0 {
			fmt.Fprintf(out, "    capped price: %.4f\n", order.MktCapPrice)
		}
	}
	fmt.Fprintln(out)
}

func renderOrderStatusText(env *Env, res *rpc.OrderStatusResult, id string) {
	out := env.Stdout
	fmt.Fprintln(out)
	if !res.Found {
		fmt.Fprintf(out, "IBKR Order Status  %s\n\n", env.statusBadge(statusConcern{Text: "NOT FOUND", Level: statusConcernWarn}))
		fmt.Fprintf(out, "No locally tracked order matched %s.\n\n", id)
		return
	}
	fmt.Fprintf(out, "IBKR Order Status  %s\n", env.statusBadge(statusConcern{Text: strings.ToUpper(res.Order.LifecycleStatus), Level: orderStatusConcernLevel(res.Order.LifecycleStatus)}))
	statusRow(env, out, "Order", formatOrderViewTitle(res.Order))
	if res.Order.ReservedOrderID != 0 {
		statusRow(env, out, "Broker ID", strconv.Itoa(res.Order.ReservedOrderID))
	}
	if res.Order.PermID != 0 {
		statusRow(env, out, "Perm ID", strconv.Itoa(res.Order.PermID))
	}
	statusRow(env, out, "Account", res.Order.Account)
	statusRow(env, out, "Status", nonEmpty(res.Order.Status, res.Order.LifecycleStatus))
	statusRow(env, out, "Filled", fmt.Sprintf("%.4g / %.4g", res.Order.Filled, res.Order.Quantity))
	if res.Order.LastMessage != "" {
		statusRow(env, out, "Message", res.Order.LastMessage)
	}
	if res.Order.WhyHeld != "" {
		statusRow(env, out, "Held", res.Order.WhyHeld)
	}
	if res.Order.MktCapPrice != 0 {
		statusRow(env, out, "Capped price", fmt.Sprintf("%.4f", res.Order.MktCapPrice))
	}
	if !res.Order.UpdatedAt.IsZero() {
		statusRow(env, out, "Updated", formatOrderTime(res.Order.UpdatedAt))
	}
	if len(res.Events) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Events:")
		for _, ev := range res.Events {
			fmt.Fprintf(out, "  - %s  %s", formatOrderTime(ev.At), ev.Type)
			if ev.LifecycleStatus != "" {
				fmt.Fprintf(out, "  %s", ev.LifecycleStatus)
			}
			if ev.Message != "" {
				fmt.Fprintf(out, "  %s", ev.Message)
			}
			if ev.WhyHeld != "" {
				fmt.Fprintf(out, "  held=%s", ev.WhyHeld)
			}
			if ev.MktCapPrice != 0 {
				fmt.Fprintf(out, "  capped_price=%.4f", ev.MktCapPrice)
			}
			fmt.Fprintln(out)
		}
	}
	fmt.Fprintln(out)
}

func formatOrderViewTitle(order rpc.OrderView) string {
	id := order.OrderRef
	if id == "" && order.ReservedOrderID != 0 {
		id = strconv.Itoa(order.ReservedOrderID)
	}
	if id == "" {
		id = "unknown-order"
	}
	qty := order.Quantity
	if qty == 0 && order.Filled+order.Remaining > 0 {
		qty = order.Filled + order.Remaining
	}
	title := fmt.Sprintf("%s  %s %.4g %s %s",
		id,
		nonEmpty(order.Action, "?"),
		qty,
		nonEmpty(order.Symbol, "?"),
		nonEmpty(order.OrderType, "?"),
	)
	switch {
	case order.Trail != nil:
		title += " " + formatOrderTrail(order.Trail)
	case order.LimitPrice != 0:
		title += fmt.Sprintf(" %.4f", order.LimitPrice)
	}
	return title + " " + nonEmpty(order.TIF, "?")
}

func orderStatusConcernLevel(status string) statusConcernLevel {
	switch status {
	case rpc.OrderLifecycleFilled, rpc.OrderLifecycleCancelled:
		return statusConcernNone
	case rpc.OrderLifecycleRejected, rpc.OrderLifecycleInactive, rpc.OrderLifecycleUnknownReconcileRequired, rpc.OrderLifecycleExpiredInferred:
		return statusConcernWarn
	default:
		return statusConcernNotice
	}
}

func formatOrderTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}
