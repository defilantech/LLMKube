/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// A libhailort stand-in for the slice of the HailoRT 5.1.1 genai API that
// hailo-ollama 5.1.1 uses, built against HailoRT's own public headers. It
// emulates a Hailo-10H without the device:
//
//   HAILO_SIM_DEVICE=0              VDevice creation fails (no HAT present).
//   HAILO_SIM_DEVICE_MEMORY=<bytes> Device memory; a HEF larger than this
//                                   fails to load. Default 8 GiB.
//   HAILO_SIM_TOKEN_DELAY_MS=<ms>   Delay per generated token. Default 0.
//
// hailo-ollama renders the chat template and writes the prompt text. The
// output is deterministic: "hailo-sim:" followed by the words of the last
// user message in that text, one token per word, cut at max_generated_tokens,
// then an empty end-of-sequence token.

#include "hailo/genai/llm/llm.hpp"
#include "hailo/vdevice.hpp"

#include <chrono>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <filesystem>
#include <regex>
#include <sstream>
#include <thread>

namespace hailort {

namespace {

uint64_t env_u64(const char *name, uint64_t fallback)
{
    const char *v = std::getenv(name);
    if (v == nullptr || *v == '\0') {
        return fallback;
    }
    return std::strtoull(v, nullptr, 10);
}

// last_user_text is the last line of prompt that is neither empty nor a role
// name once special tokens (<|...|>) are removed: the last user message for
// the ChatML and Llama 3 templates hailo-ollama renders.
std::string last_user_text(const std::string &prompt)
{
    static const std::regex special("<\\|[^|]*\\|>");
    std::istringstream lines(std::regex_replace(prompt, special, "\n"));
    std::string last;
    for (std::string line; std::getline(lines, line);) {
        const auto b = line.find_first_not_of(" \t\r");
        if (b == std::string::npos) {
            continue;
        }
        line = line.substr(b, line.find_last_not_of(" \t\r") - b + 1);
        if (line == "system" || line == "user" || line == "assistant") {
            continue;
        }
        last = line;
    }
    return last;
}

} // namespace

Expected<std::shared_ptr<VDevice>> VDevice::create_shared()
{
    if (env_u64("HAILO_SIM_DEVICE", 1) == 0) {
        return make_unexpected(HAILO_OUT_OF_PHYSICAL_DEVICES);
    }
    // LLM::create below never dereferences the device.
    return std::shared_ptr<VDevice>();
}

namespace genai {

LLMParams::LLMParams(const std::string &hef_path, const std::string &lora_name, bool optimize_memory_on_device) :
    m_hef_path(hef_path), m_lora(lora_name), m_optimize_memory_on_device(optimize_memory_on_device)
{}

hailo_status LLMParams::set_model(const std::string &hef_path, const std::string &lora_name)
{
    m_hef_path = hef_path;
    m_lora = lora_name;
    return HAILO_SUCCESS;
}

const std::string &LLMParams::hef() const { return m_hef_path; }
const std::string &LLMParams::lora() const { return m_lora; }
bool LLMParams::optimize_memory_on_device() const { return m_optimize_memory_on_device; }
void LLMParams::set_optimize_memory_on_device(bool v) { m_optimize_memory_on_device = v; }

hailo_status LLMGeneratorParams::set_temperature(float32_t v) { m_temperature = v; return HAILO_SUCCESS; }
float32_t LLMGeneratorParams::temperature() const { return m_temperature; }
hailo_status LLMGeneratorParams::set_top_p(float32_t v) { m_top_p = v; return HAILO_SUCCESS; }
float32_t LLMGeneratorParams::top_p() const { return m_top_p; }
hailo_status LLMGeneratorParams::set_top_k(uint32_t v) { m_top_k = v; return HAILO_SUCCESS; }
uint32_t LLMGeneratorParams::top_k() const { return m_top_k; }
hailo_status LLMGeneratorParams::set_frequency_penalty(float32_t v) { m_frequency_penalty = v; return HAILO_SUCCESS; }
float32_t LLMGeneratorParams::frequency_penalty() const { return m_frequency_penalty; }
hailo_status LLMGeneratorParams::set_max_generated_tokens(uint32_t v) { m_max_generated_tokens = v; return HAILO_SUCCESS; }
uint32_t LLMGeneratorParams::max_generated_tokens() const { return m_max_generated_tokens; }
hailo_status LLMGeneratorParams::set_do_sample(bool v) { m_do_sample = v; return HAILO_SUCCESS; }
bool LLMGeneratorParams::do_sample() const { return m_do_sample; }
hailo_status LLMGeneratorParams::set_seed(uint32_t v) { m_seed = v; return HAILO_SUCCESS; }
uint32_t LLMGeneratorParams::seed() const { return m_seed; }

class LLMGeneratorCompletion::Impl
{
public:
    Impl(std::deque<std::string> tokens, bool truncated, std::chrono::milliseconds delay) :
        m_tokens(std::move(tokens)), m_truncated(truncated), m_delay(delay)
    {}

    Expected<std::string> read()
    {
        if (m_tokens.empty()) {
            return make_unexpected(HAILO_INVALID_OPERATION);
        }
        std::this_thread::sleep_for(m_delay);
        auto token = std::move(m_tokens.front());
        m_tokens.pop_front();
        return token;
    }

    Status status() const
    {
        if (m_aborted) {
            return Status::ABORTED;
        }
        if (!m_tokens.empty()) {
            return Status::GENERATING;
        }
        return m_truncated ? Status::MAX_TOKENS_REACHED : Status::LOGICAL_END_OF_GENERATION;
    }

    void abort() { m_aborted = true; m_tokens.clear(); }

private:
    std::deque<std::string> m_tokens;
    bool m_truncated;
    std::chrono::milliseconds m_delay;
    bool m_aborted = false;
};

LLMGeneratorCompletion::LLMGeneratorCompletion(std::unique_ptr<Impl> pimpl) : m_pimpl(std::move(pimpl)) {}
LLMGeneratorCompletion::LLMGeneratorCompletion(LLMGeneratorCompletion &&) = default;
LLMGeneratorCompletion::~LLMGeneratorCompletion() = default;

Expected<std::string> LLMGeneratorCompletion::read(std::chrono::milliseconds) { return m_pimpl->read(); }

Expected<std::string> LLMGeneratorCompletion::read_all(std::chrono::milliseconds)
{
    std::string out;
    while (m_pimpl->status() == Status::GENERATING) {
        auto token = m_pimpl->read();
        if (!token) {
            return make_unexpected(token.status());
        }
        out += token.value();
    }
    return out;
}

Expected<size_t> LLMGeneratorCompletion::read(char *output, size_t output_size, std::chrono::milliseconds)
{
    auto token = m_pimpl->read();
    if (!token) {
        return make_unexpected(token.status());
    }
    const auto n = std::min(output_size, token.value().size());
    std::memcpy(output, token.value().data(), n);
    return size_t(n);
}

LLMGeneratorCompletion::Status LLMGeneratorCompletion::generation_status() const { return m_pimpl->status(); }

hailo_status LLMGeneratorCompletion::abort()
{
    m_pimpl->abort();
    return HAILO_SUCCESS;
}

class LLMGenerator::Impl
{
public:
    explicit Impl(LLMGeneratorParams params) : m_params(std::move(params)) {}

    hailo_status write(const std::string &text)
    {
        m_prompt += text;
        return HAILO_SUCCESS;
    }

    Expected<LLMGeneratorCompletion> generate()
    {
        std::deque<std::string> tokens{"hailo-sim:"};
        std::istringstream words(last_user_text(m_prompt));
        for (std::string w; words >> w;) {
            tokens.push_back(" " + w);
        }
        const auto max = m_params.max_generated_tokens();
        const bool truncated = (max > 0) && (tokens.size() > max);
        if (truncated) {
            tokens.resize(max);
        } else {
            // The end-of-sequence token a model ends with; hailo-ollama never
            // returns a completion's last token.
            tokens.push_back("");
        }
        const std::chrono::milliseconds delay(env_u64("HAILO_SIM_TOKEN_DELAY_MS", 0));
        return LLMGeneratorCompletion(
            std::make_unique<LLMGeneratorCompletion::Impl>(std::move(tokens), truncated, delay));
    }

private:
    LLMGeneratorParams m_params;
    std::string m_prompt;
};

LLMGenerator::LLMGenerator(std::shared_ptr<Impl> pimpl) : m_pimpl(std::move(pimpl)) {}
LLMGenerator::LLMGenerator(LLMGenerator &&other) = default;
LLMGenerator::~LLMGenerator() = default;

hailo_status LLMGenerator::write(const std::string &prompt) { return m_pimpl->write(prompt); }

hailo_status LLMGenerator::write(const std::vector<std::string> &prompt_json_strings)
{
    for (const auto &s : prompt_json_strings) {
        m_pimpl->write(s + "\n");
    }
    return HAILO_SUCCESS;
}

Expected<LLMGeneratorCompletion> LLMGenerator::generate() { return m_pimpl->generate(); }

class LLM::Impl
{
};

Expected<LLM> LLM::create(std::shared_ptr<VDevice>, const LLMParams &llm_params)
{
    std::error_code ec;
    const auto size = std::filesystem::file_size(llm_params.hef(), ec);
    if (ec) {
        return make_unexpected(HAILO_NOT_FOUND);
    }
    if (size > env_u64("HAILO_SIM_DEVICE_MEMORY", uint64_t(8) << 30)) {
        // The status a real Hailo-10H returns for a HEF that does not fit is unverified.
        return make_unexpected(HAILO_OUT_OF_HOST_MEMORY);
    }
    return LLM(std::make_unique<Impl>());
}

LLM::LLM(std::unique_ptr<Impl> pimpl) : m_pimpl(std::move(pimpl)) {}
LLM::LLM(LLM &&) = default;
LLM::~LLM() = default;

Expected<LLMGeneratorParams> LLM::create_generator_params()
{
    return LLMGeneratorParams(0.7f, 0.8f, 20, 1.1f, 512, true, 0);
}

Expected<LLMGenerator> LLM::create_generator(const LLMGeneratorParams &params)
{
    return LLMGenerator(std::make_shared<LLMGenerator::Impl>(params));
}

Expected<LLMGenerator> LLM::create_generator()
{
    auto params = create_generator_params();
    return create_generator(params.value());
}

hailo_status LLM::clear_context() { return HAILO_SUCCESS; }

} // namespace genai
} // namespace hailort
