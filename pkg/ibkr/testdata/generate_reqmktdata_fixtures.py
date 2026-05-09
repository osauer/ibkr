#!/usr/bin/env python3
"""Generate reqMktData wire payload fixtures using the official IB Python client.

This script builds sample requests for common contract shapes (stock, index,
option) with a modern server version so we can regression-test our Go encoder
against canonical output.
"""

import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[4]
IBPY_ROOT = Path("/Users/osauer/twsapi_macunix/IBJts/source/pythonclient")
if not IBPY_ROOT.exists():
    raise SystemExit(f"IB API pythonclient not found: {IBPY_ROOT}")

sys.path.insert(0, str(IBPY_ROOT))

from ibapi.client import EClient
from ibapi.wrapper import EWrapper
from ibapi.contract import Contract


class _CaptureConn:
    def __init__(self):
        self.messages = []

    def isConnected(self):
        return True

    def sendMsg(self, msg: bytes):
        self.messages.append(msg)


class _CaptureClient(EClient):
    def __init__(self, server_version: int):
        super().__init__(EWrapper())
        self.conn = _CaptureConn()
        self.setConnState(EClient.CONNECTED)
        self.serverVersion_ = server_version

    @property
    def captured(self):
        return self.conn.messages


def _build_stock(server_version: int) -> bytes:
    client = _CaptureClient(server_version)
    contract = Contract()
    contract.symbol = "SPY"
    contract.secType = "STK"
    contract.exchange = "SMART"
    contract.currency = "USD"
    contract.primaryExchange = ""

    client.reqMktData(
        reqId=1,
        contract=contract,
        genericTickList="100,101,104,165,221,233",
        snapshot=False,
        regulatorySnapshot=False,
        mktDataOptions=[],
    )
    return client.captured[-1]


def _build_index(server_version: int) -> bytes:
    client = _CaptureClient(server_version)
    contract = Contract()
    contract.symbol = "VIX"
    contract.secType = "IND"
    contract.exchange = "CBOE"
    contract.currency = "USD"
    contract.primaryExchange = "CBOE"
    contract.localSymbol = "VIX"
    contract.tradingClass = "VIX"

    client.reqMktData(
        reqId=2,
        contract=contract,
        genericTickList="100,101,104,165,221,233",
        snapshot=False,
        regulatorySnapshot=False,
        mktDataOptions=[],
    )
    return client.captured[-1]


def _build_option(server_version: int) -> bytes:
    client = _CaptureClient(server_version)
    contract = Contract()
    contract.symbol = "SPY"
    contract.secType = "OPT"
    contract.lastTradeDateOrContractMonth = "20251219"
    contract.strike = 400
    contract.right = "P"
    contract.multiplier = "100"
    contract.conId = 777001
    contract.exchange = "SMART"
    contract.primaryExchange = "CBOE"
    contract.currency = "USD"

    client.reqMktData(
        reqId=3,
        contract=contract,
        genericTickList="100,101,104,106,221",
        snapshot=False,
        regulatorySnapshot=False,
        mktDataOptions=[],
    )
    return client.captured[-1]


def main():
    out_dir = ROOT / "internal" / "components" / "ibkr" / "testdata"
    out_dir.mkdir(parents=True, exist_ok=True)

    server_version = 176

    fixtures = {
        "reqmktdata_stock_sv176.bin": _build_stock(server_version),
        "reqmktdata_index_sv176.bin": _build_index(server_version),
        "reqmktdata_option_sv176.bin": _build_option(server_version),
    }

    import struct

    for name, payload in fixtures.items():
        size = struct.unpack("!I", payload[:4])[0]
        body = payload[4:4 + size]
        fields = body.split(b"\0")

        if len(fields) > 0:
            # drop trailing empty to manipulate
            trailing = fields[-1] == b""
            core = fields[:-1] if trailing else fields

            msg_type = int(core[0])
            remainder = core[1:]
            if trailing:
                remainder.append(b"")

            ascii_body = b"\0".join(remainder)
            body = struct.pack("!I", msg_type) + ascii_body
            payload = struct.pack("!I", len(body)) + body

        path = out_dir / name
        path.write_bytes(payload)
        print(f"wrote {path.relative_to(ROOT)} ({len(payload)} bytes)")


if __name__ == "__main__":
    main()
