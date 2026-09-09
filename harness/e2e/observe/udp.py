"""Evaluate raw agnhost /dial output without hiding failed UDP attempts."""

import argparse
import json
import pathlib
import subprocess


def evaluate(body, payload, tries, exit_code=0):
    """Count reported echoes, not inferred delivered packets.

    agnhost may omit all responses when its final attempt fails, including
    responses to earlier successful attempts. Missing responses cannot be
    interpreted as an exact loss count.
    """
    if type(tries) is not int or tries < 1 or not isinstance(payload, str) or not payload:
        raise ValueError("positive tries and a nonempty expected payload are required")
    result = dict(ok=False, expectedResponses=tries, reportedResponses=0,
                  exactResponses=0, responsesPresent=False, errors=[],
                  commandExitCode=exit_code)
    try:
        data = json.loads(body.lstrip("\ufeff"))
        if not isinstance(data, dict):
            raise ValueError("dial output must be an object")
        responses = data.get("responses", [])
        errors = data.get("errors", [])
        for name, values in (("responses", responses), ("errors", errors)):
            if not isinstance(values, list) or any(not isinstance(v, str) for v in values):
                raise ValueError(name + " must be an array of strings")
    except ValueError as error:
        result["parseError"] = str(error)
        return result
    result.update(responsesPresent="responses" in data,
                  reportedResponses=len(responses),
                  exactResponses=sum(response == payload for response in responses),
                  errors=errors)
    result["ok"] = (exit_code == 0 and not errors and len(responses) == tries
                    and result["exactResponses"] == tries)
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--payload-bytes", type=int, required=True,
                        help="expected lowercase x payload length (1..2043)")
    parser.add_argument("--tries", type=int, required=True)
    parser.add_argument("--body", type=pathlib.Path, help="replay a saved raw JSON body")
    parser.add_argument("--exit-code", type=int, default=0, help="saved command status for --body")
    parser.add_argument("--timeout", type=float, default=90)
    parser.add_argument("command", nargs=argparse.REMAINDER,
                        help="after --, node executor and curl command producing raw /dial JSON")
    args = parser.parse_args(argv)
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not 1 <= args.payload_bytes <= 2043 or args.tries < 1 or args.timeout <= 0:
        parser.error("require payload 1..2043, positive tries and timeout")
    if bool(args.body) == bool(command):
        parser.error("specify either --body or an executor command")
    if command and args.exit_code:
        parser.error("--exit-code applies only to saved bodies")
    if args.body:
        body, code = args.body.read_text(encoding="utf-8-sig"), args.exit_code
    else:
        try:
            run = subprocess.run(command, capture_output=True, text=True,
                                 encoding="utf-8-sig", timeout=args.timeout)
            body, code = run.stdout, run.returncode
        except subprocess.TimeoutExpired:
            print(json.dumps(dict(ok=False, transportError="executor timed out")))
            return 1
        except OSError:
            print(json.dumps(dict(ok=False, transportError="executor could not start")))
            return 1
    result = evaluate(body, "x" * args.payload_bytes, args.tries, code)
    print(json.dumps(result))
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
