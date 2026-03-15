--- prowl.yazi — Yazi previewer plugin for prowl context
--- Shows structural context (exports, signatures, calls, callers, community)
--- for source files in prowl-indexed projects.

local M = {}

-- Find .prowl/context directory by walking up from the file's directory
local function find_context_dir(path)
	local dir = path:match("(.+)/[^/]+$") or "."
	while dir and dir ~= "" and dir ~= "/" do
		local prowl_dir = dir .. "/.prowl/context"
		local f = io.open(prowl_dir .. "/_meta/index.txt", "r")
		if f then
			f:close()
			return prowl_dir, dir
		end
		dir = dir:match("(.+)/[^/]+$")
	end
	return nil, nil
end

-- Read a context dotfile (e.g., .exports, .signatures)
local function read_dotfile(context_dir, rel_path, name)
	local path = context_dir .. "/" .. rel_path .. "/" .. name
	local f = io.open(path, "r")
	if not f then return nil end
	local content = f:read("*a")
	f:close()
	if content and content ~= "" then
		return content
	end
	return nil
end

-- Supported source file extensions
local SOURCE_EXTS = {
	[".go"] = true,
	[".ts"] = true, [".tsx"] = true,
	[".js"] = true, [".jsx"] = true,
	[".rs"] = true,
	[".py"] = true,
	[".java"] = true,
	[".c"] = true, [".cpp"] = true, [".h"] = true, [".hpp"] = true,
	[".cs"] = true,
	[".swift"] = true,
	[".rb"] = true,
}

local function get_ext(path)
	return path:match("(%.[^%.]+)$")
end

function M.peek(self)
	local url = tostring(self.file.url)
	local ext = get_ext(url)

	if not ext or not SOURCE_EXTS[ext:lower()] then
		-- Not a source file, fall through to default previewer
		return 0
	end

	local context_dir, project_root = find_context_dir(url)
	if not context_dir or not project_root then
		return 0 -- No prowl index found
	end

	-- Get relative path from project root
	local rel_path = url:sub(#project_root + 2) -- +2 for the trailing /

	local lines = {}

	-- Header
	table.insert(lines, "╭─ prowl context ─────────────────────")
	table.insert(lines, "│")

	-- Community
	local community = read_dotfile(context_dir, rel_path, ".community")
	if community then
		table.insert(lines, "│ ◆ Community")
		for line in community:gmatch("[^\n]+") do
			table.insert(lines, "│   " .. line)
		end
		table.insert(lines, "│")
	end

	-- Exports
	local exports = read_dotfile(context_dir, rel_path, ".exports")
	if exports then
		table.insert(lines, "│ ▸ Exports")
		local count = 0
		for line in exports:gmatch("[^\n]+") do
			if count < 15 then
				table.insert(lines, "│   " .. line)
				count = count + 1
			else
				table.insert(lines, "│   ... and more")
				break
			end
		end
		table.insert(lines, "│")
	end

	-- Signatures
	local signatures = read_dotfile(context_dir, rel_path, ".signatures")
	if signatures then
		table.insert(lines, "│ ƒ Signatures")
		local count = 0
		for line in signatures:gmatch("[^\n]+") do
			if count < 15 then
				table.insert(lines, "│   " .. line)
				count = count + 1
			else
				table.insert(lines, "│   ... and more")
				break
			end
		end
		table.insert(lines, "│")
	end

	-- Calls
	local calls = read_dotfile(context_dir, rel_path, ".calls")
	if calls then
		table.insert(lines, "│ → Calls")
		local count = 0
		for line in calls:gmatch("[^\n]+") do
			if count < 10 then
				table.insert(lines, "│   " .. line)
				count = count + 1
			else
				table.insert(lines, "│   ... and more")
				break
			end
		end
		table.insert(lines, "│")
	end

	-- Callers
	local callers = read_dotfile(context_dir, rel_path, ".callers")
	if callers then
		table.insert(lines, "│ ← Callers")
		local count = 0
		for line in callers:gmatch("[^\n]+") do
			if count < 10 then
				table.insert(lines, "│   " .. line)
				count = count + 1
			else
				table.insert(lines, "│   ... and more")
				break
			end
		end
		table.insert(lines, "│")
	end

	-- Upstream (imported by)
	local upstream = read_dotfile(context_dir, rel_path, ".upstream")
	if upstream then
		table.insert(lines, "│ ↑ Imported by")
		local count = 0
		for line in upstream:gmatch("[^\n]+") do
			if count < 10 then
				table.insert(lines, "│   " .. line)
				count = count + 1
			else
				table.insert(lines, "│   ... and more")
				break
			end
		end
	end

	table.insert(lines, "│")
	table.insert(lines, "╰──────────────────────────────────────")

	if #lines <= 4 then
		-- No context data found for this file
		return 0
	end

	ya.preview_widgets(self, {
		ui.Text(lines)
			:area(self.area)
	})
end

function M.seek(self, units)
	-- No scrolling support needed for now
end

return M
