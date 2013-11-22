-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at http://mozilla.org/MPL/2.0/.

-- sample input
---------------
-- {"url":"/submit/sample","duration_ms":0.547324,"code":200,"size":4819,"level":"info","message":"OK","timestamp":"2013-09-10T20:43:17.217Z"}

-- Injected Heka message
------------------------
--	Timestamp: 2013-09-10 20:43:17.216999936 +0000 UTC
--	Type: TelemetryServerLog
--	Hostname: trink-x230
--	Pid: 0
--	UUID: 2be3ed98-89e8-4bd0-a7c4-9aebe8747a8b
--	Logger: jsonshort.log
--	Payload: 
--	EnvVersion: 
--	Severity: 6
--	Fields: [
--	name:"message" value_string:"OK"  
--	name:"code" value_type:DOUBLE value_double:200  
--	name:"url" value_string:"/submit/sample"  
--	name:"duration" value_type:DOUBLE representation:"ms" value_double:0.547324  
--	name:"size" value_type:DOUBLE representation:"B" value_double:4819 ]

require "os"
require "cjson"
require "lpeg"
local rfc3339 = require("rfc3339")
local severity = require("rfc5424_severity")

local fields = {
    url = "",
    duration = {value="", representation="ms"},
    code = "",
    size = {value="", representation="B"},
    message = ""
}

local msg = {
    Type = "TelemetryServerLog",
    Severity = "7",
    Fields = fields
}

function process_message()
    json = cjson.decode(read_message("Payload"))
    if not json then
        return -1
    end

    local ts = lpeg.match(rfc3339, json.timestamp)
    if ts then
        local offset = 0
        if ts.hour_offset then
            offset = (ts.hour_offset * 60 * 60) + (ts.minute_offset * 60)
            if ts.sign_offset == "+" then offset = offset * -1 end
        end

        local frac = 0
        if ts.sec_frac then
            frac = ts.sec_frac
        end
        -- for this to work properly the Heka TZ must be UTC
        msg.Timestamp = (os.time(ts) + frac + offset) * 1e9
    end

    msg.Severity = lpeg.match(severity, json.level) or "7"
    fields.url = json.url
    fields.duration.value = json.duration_ms
    fields.code = json.code
    fields.size.value = json.size
    fields.message = json.message

    inject_message(msg)
    return 0
end
