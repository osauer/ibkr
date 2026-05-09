#!/usr/bin/env python3
"""Generate reqContractData wire payload fixture using the official IB Python client."""

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


def _build_stock_contract(server_version: int) -> bytes:
    client = _CaptureClient(server_version)
    contract = Contract()
    contract.symbol = "IWM"
    contract.secType = "STK"
    contract.exchange = "SMART"
    contract.currency = "USD"
    contract.primaryExchange = ""

    client.reqContractDetails(10, contract)
    return client.captured[-1]


def main():
    server_version = 176

    payload = _build_stock_contract(server_version)

    import struct

    # Parse the payload
    size = struct.unpack("!I", payload[:4])[0]
    body = payload[4:4 + size]
    fields = body.split(b"\0")

    print(f"Total payload size: {len(payload)} bytes")
    print(f"Message body size: {size} bytes")
    print(f"\nField breakdown:")
    for i, field in enumerate(fields):
        if field:
            try:
                decoded = field.decode('ascii')
                print(f"  [{i}]: {repr(decoded)} ({field.hex()})")
            except:
                print(f"  [{i}]: <binary> ({field.hex()})")
        else:
            print(f"  [{i}]: <empty>")


if __name__ == "__main__":
    main()
