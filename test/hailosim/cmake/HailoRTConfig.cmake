# Copyright 2026 The LLMKube Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

get_filename_component(_hailosim_prefix "${CMAKE_CURRENT_LIST_DIR}/../../.." ABSOLUTE)

if(NOT TARGET HailoRT::libhailort)
    add_library(HailoRT::libhailort SHARED IMPORTED)
    set_target_properties(HailoRT::libhailort PROPERTIES
        IMPORTED_LOCATION "${_hailosim_prefix}/lib/libhailort.so"
        INTERFACE_INCLUDE_DIRECTORIES "${_hailosim_prefix}/include")
endif()

set(HailoRT_FOUND TRUE)
