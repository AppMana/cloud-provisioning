#!/usr/bin/env python3
"""Derive the windows-gpu-device-plugin image using the shared Windows layer builder."""
import pathlib
import sys
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import derive_windows as common

checked_binary = common.checked_binary
run = common.run

def write_binary_layer(layer, raw):
    return common.write_binary_layer(layer, raw, 'device-plugin-wddm.exe')

def build(base, binary, expected, output):
    return common.build(base, binary, expected, output, 'device-plugin-wddm.exe', 'windows-gpu-device-plugin')

if __name__ == '__main__':
    common.main('device-plugin-wddm.exe', 'windows-gpu-device-plugin')
